package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// nonStreamBody is a chat request with no "stream" key at all, which is what
// most OpenAI clients send by default.
func nonStreamBody(model string) string {
	b, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	return string(b)
}

// recommendedJSON mirrors what Cline's endpoint actually returns: a "free"
// group alongside groups this proxy must leave out.
const recommendedJSON = `{
  "recommended": [
    {"id":"anthropic/claude-sonnet-5.5","name":"claude-sonnet-5.5","description":"","tags":["NEW"]}
  ],
  "free": [
    {"id":"cline-free/deepseek-v4.1-flash","name":"Deepseek-v4.1-Flash","description":"1M context","tags":[]},
    {"id":"stealth/pixel-canary","name":"Pixel Canary","description":"","tags":[]}
  ],
  "clinePass": [
    {"id":"cline-pass/glm-5.3","name":"glm-5.3","description":"","tags":[]}
  ]
}`

// modelsStub answers the recommended-models path and records how it was called.
type modelsStub struct {
	url string

	mu     sync.Mutex
	calls  int
	tokens []string
	status int
	body   string
}

func newModelsStub(t *testing.T, body string) *modelsStub {
	t.Helper()
	s := &modelsStub{status: http.StatusOK, body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1"+modelsPath {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		s.calls++
		s.tokens = append(s.tokens, r.Header.Get("Authorization"))
		code, body := s.status, s.body
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL + "/api/v1"
	return s
}

func (s *modelsStub) set(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.body = status, body
}

func (s *modelsStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// leakedToken reports whether any API key travelled on a list request.
func (s *modelsStub) leakedToken() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, tok := range s.tokens {
		if tok != "" {
			return true
		}
	}
	return false
}

type modelList struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

func TestModelsServesTheFreeGroupInOpenAIShape(t *testing.T) {
	up := newModelsStub(t, recommendedJSON)
	p, _ := newProxyAt(t, up.url, 3, "k0")

	resp, out := get(t, p.URL+"/v1/models", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, out)
	}
	var got modelList
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "list" {
		t.Errorf("object = %q, want list", got.Object)
	}

	// Only the free group, in its order, each a proper OpenAI model object.
	want := []struct{ id, owner string }{
		{"cline-free/deepseek-v4.1-flash", "cline-free"},
		{"stealth/pixel-canary", "stealth"},
	}
	if len(got.Data) != len(want) {
		t.Fatalf("data = %+v, want the %d free models only", got.Data, len(want))
	}
	for i, w := range want {
		if got.Data[i].ID != w.id || got.Data[i].OwnedBy != w.owner || got.Data[i].Object != "model" {
			t.Errorf("data[%d] = %+v, want id %q owned by %q, object model", i, got.Data[i], w.id, w.owner)
		}
	}
}

// The list is public, so no key should leave the pool for it -- and a caller
// polling /v1/models must not disturb which key a model is on.
func TestModelsSpendsNoKey(t *testing.T) {
	up := newModelsStub(t, recommendedJSON)
	p, pr := newProxyAt(t, up.url, 3, "k0")

	get(t, p.URL+"/v1/models", "")
	if up.leakedToken() {
		t.Error("an API key travelled on a list request")
	}
	if got := pr.pool.order("anything", time.Now()); len(got) != 1 {
		t.Errorf("pool order = %v, want it untouched", got)
	}
}

func TestModelsIsFetchedOnceAndCached(t *testing.T) {
	up := newModelsStub(t, recommendedJSON)
	p, _ := newProxyAt(t, up.url, 3, "k0")

	for range 3 {
		if resp, _ := get(t, p.URL+"/v1/models", ""); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	}
	if got := up.count(); got != 1 {
		t.Errorf("upstream saw %d calls, want the list fetched once", got)
	}
}

// Clients read this at startup, so a momentary upstream failure should serve
// the last good copy rather than leave them with no models at all.
func TestModelsFallsBackToTheCacheWhenUpstreamFails(t *testing.T) {
	up := newModelsStub(t, recommendedJSON)
	p, pr := newProxyAt(t, up.url, 3, "k0")

	_, first := get(t, p.URL+"/v1/models", "")
	if len(first) == 0 {
		t.Fatal("no first listing to fall back on")
	}

	pr.models.ttl = 0 // force the next call to refetch
	up.set(http.StatusInternalServerError, `{"error":"boom"}`)

	resp, out := get(t, p.URL+"/v1/models", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the cached list rather than an error", resp.StatusCode)
	}
	if !bytes.Equal(first, out) {
		t.Errorf("body changed after the refresh failed:\n%s", out)
	}
}

// With nothing cached there is nothing to serve, so the failure is reported.
func TestModelsReportsAnErrorWhenNothingIsCached(t *testing.T) {
	up := newModelsStub(t, "")
	up.set(http.StatusInternalServerError, "")

	p, _ := newProxyAt(t, up.url, 3, "k0")
	resp, _ := get(t, p.URL+"/v1/models", "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestModelsRejectsNonGet(t *testing.T) {
	up := newModelsStub(t, recommendedJSON)
	p, _ := newProxyAt(t, up.url, 3, "k0")

	if resp, _ := post(t, p.URL+"/v1/models", "", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	if got := up.count(); got != 0 {
		t.Errorf("upstream saw %d calls, want none", got)
	}
}

// A stream:false request cannot be answered by Cline's gateway, which streams
// only. It is refused before a key is spent on it. An omitted stream is false
// by the OpenAI default, so it lands here too.
func TestNonStreamingRequestsAreRefused(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"omitted", nonStreamBody(deepseek)},
		{"explicit false", `{"model":"` + deepseek + `","messages":[],"stream":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &rec{}
			up := upstream(t, r, func(w http.ResponseWriter, _, _ string) { ok(w) })
			p := newProxy(t, up, "k0")

			resp, out := post(t, p.URL+"/v1/chat/completions", "", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if !strings.Contains(string(out), "非流式") {
				t.Errorf("body = %s, want the non-stream explanation", out)
			}
			if got := len(r.keys()); got != 0 {
				t.Errorf("upstream saw %d requests, want none: no key for a shape Cline cannot answer", got)
			}
		})
	}
}
