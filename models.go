package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// modelsPath is relative to the API base, same as /chat/completions.
const modelsPath = "/ai/cline/recommended-models"

const (
	// modelsTTL is how long a fetched list is served before it is fetched
	// again. The list changes when Cline ships a model, not per request.
	modelsTTL = 10 * time.Minute
	// modelsTimeout bounds the fetch, and the caller waits on it.
	modelsTimeout = 15 * time.Second
)

// modelCache serves the free half of Cline's recommended-models list, shaped
// the way an OpenAI client expects it from GET /v1/models.
//
// The list is cached, and a failed refresh falls back to the last good copy:
// clients fetch this at startup, so a momentary upstream hiccup should not be
// the reason they see no models at all.
type modelCache struct {
	base   *url.URL
	client *http.Client
	ttl    time.Duration

	mu   sync.Mutex
	list []byte    // the JSON last handed out
	at   time.Time // when it was fetched
}

func newModelCache(base *url.URL, client *http.Client) *modelCache {
	return &modelCache{base: base, client: client, ttl: modelsTTL}
}

// get returns the OpenAI-shaped model list, refetching when the copy in hand
// has aged out. A failed refresh serves the stale copy rather than an error.
func (m *modelCache) get(ctx context.Context) ([]byte, error) {
	m.mu.Lock()
	cached, at := m.list, m.at
	m.mu.Unlock()

	if cached != nil && time.Since(at) < m.ttl {
		return cached, nil
	}

	fresh, err := m.fetch(ctx)
	if err != nil {
		if cached != nil {
			log.Printf("models: refresh failed, serving the cached list: %v", err)
			return cached, nil
		}
		return nil, err
	}

	m.mu.Lock()
	m.list, m.at = fresh, time.Now()
	m.mu.Unlock()
	return fresh, nil
}

// recommendedModel is one entry of the upstream reply. Every group has the
// same shape; only the fields we pass on are read.
type recommendedModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type recommended struct {
	Free []recommendedModel `json:"free"`
}

func (m *modelCache) fetch(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint(m.base, modelsPath), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	// No Authorization: the list is public, and a key has no business leaving
	// home for a call that does not check one.

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	// maxBody is far past this reply, and keeps a rogue upstream bounded.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}

	var rec recommended
	if err := json.Unmarshal(body, &rec); err != nil {
		return nil, fmt.Errorf("unreadable recommended-models: %w", err)
	}
	return openAIList(rec.Free)
}

// openAIModel is the subset of the OpenAI model object clients actually read.
type openAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func openAIList(free []recommendedModel) ([]byte, error) {
	created := time.Now().Unix()
	// Non-nil, so an empty group marshals as [] rather than null.
	out := make([]openAIModel, 0, len(free))
	for _, m := range free {
		if m.ID == "" {
			continue
		}
		out = append(out, openAIModel{
			ID:      m.ID,
			Object:  "model",
			Created: created,
			OwnedBy: owner(m.ID),
		})
	}
	return json.Marshal(map[string]any{"object": "list", "data": out})
}

// owner is the vendor half of a namespaced id, so a client that groups by
// owned_by groups by maker instead of lumping everything under "cline-free".
func owner(id string) string {
	if vendor, _, ok := strings.Cut(id, "/"); ok && vendor != "" {
		return vendor
	}
	return "cline"
}
