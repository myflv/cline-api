# cline-api

把一组 Cline API key 放在一个 OpenAI 兼容端点后面，429 时自动换号。上游固定是 Cline（`https://api.cline.bot/api/v1`）。

调用者只管发请求，密钥由代理挑。

## 只支持流式请求

**这个渠道只回答 SSE，所以只接受 `stream: true`。**

请求体里没写 `stream`（按 OpenAI 的默认值就是 false）或者显式写了 `stream: false`，代理直接返回 400：

```json
{"error":{"code":400,"message":"当前渠道不支持非流式请求，请设置 stream: true","type":"proxy_error"}}
```

这一步在挑 key 之前做，**不会消耗任何额度** —— 与其让一个 key 去产出一个上游根本给不出的响应，不如当场说清楚。

## 一个模型固定用一个 key，429 才换

**默认不换号。** 一个模型会一直用它当前的那个 key，直到这个 key 对它返回 429 —— 这时才把该 key 针对**该模型**冷却，并把这次请求交给下一个 key。

这么设计是为了缓存：上游的 prompt cache 是按账号算的，每个请求都换个 key 等于每次都把缓存打散。会话粘在一个 key 上，命中率才上得去。

换号之后也**不会自动挪回来**。某个 key 冷却结束、重新可用后，它只是回到候选队列里；模型仍待在当前这个能干活的 key 上 —— 挪回去又是一次白白丢弃缓存的切换。只有在当前 key 再次 429 时才继续往后走，走到底就绕回开头。

## 冷却只绑在 (key, 模型) 上

额度是按 key、按模型分开算的，所以冷却也这样记账：

- key 3 的 `cline-free/deepseek-v4.1-flash` 额度用光了 → 只有「key 3 + deepseek-v4.1-flash」被停用
- key 3 的 `cline-pass/glm-5.3` 不受影响：它继续用 key 3，**也不会被拽到别的 key 上**

模型名取自请求体里的 `model` 字段，原样匹配。

## 冷却时间

| 配置 | 默认 | 含义 |
| --- | --- | --- |
| `cooldown` | `60s` | 首次 429 后停用多久 |
| `cooldown_max` | `30m` | 连续 429 时翻倍的上限 |

撞到 429 先停 60s。**等它真的恢复之后又 429**（说明是更长周期的额度，不是每分钟限流），就翻倍：60s → 120s → 240s …… 到 `cooldown_max` 为止。**一旦成功，计数清零**，不会因为历史记录被一直惩罚。

注意翻倍的门槛是「恢复之后又 429」。所有 key 都在冷却时，代理会拿恢复最早的那个再试一次；这种补试失败**什么都不改变** —— 不加倍，也不推迟它原本的恢复时刻。最后这点很要紧：补试要是把恢复时刻往后推，那只要请求持续不断，被限流的 key 就永远回不来了。

反过来，**恢复之后空闲超过 `cooldown_max`（默认 30 分钟）就从头算** —— 这个计数衡量的是「它一直在失败」，不是「它历史上失败过几次」。停 60s → 恢复 → 马上又 429，是同一轮（下次 2m）；闲了一个小时再 429，就从头来（还是 60s）。

**为什么默认 60s**：够长，能跨过典型的每分钟限流窗口；够短，不会把只是短暂抖动的 key 闲置太久。真遇到长周期额度，翻倍会自动退避，不需要你调参。

## 只有 429 会换 key

| 上游返回 | 代理行为 |
| --- | --- |
| `429` | 冷却该 key+模型，还有额度就换下一个 key 重试 |
| `200` | 原样透传（SSE 流） |
| 其它一切（400/401/403/5xx/网络错误） | 原样返回给调用者 |

其它状态码换 key 没有意义：同一个域名、同一个请求，下一个 key 只会得到同样的答案，白白让调用者多等几轮。

重试**只发生在响应头到达之后、任何字节写给调用者之前**。所以 SSE 请求能安全重试，不会吐出半截流。

想要「拿到 429 就直接回给调用者，一次都不重试」，设 `max_attempts: 1`。

用尽 `max_attempts` 之后，代理把**上游自己的那个 429 原样交回**，只是额外附上 `Retry-After`（如果池子知道什么时候有 key 空闲）。代理不会替你编一句「所有 key 都被限流」——`max_attempts` 截断循环时那句话往往是假的，而上游的原文（比如「额度到 14:30 恢复」）比它有用得多。

## /v1/models 列出免费模型

`GET /v1/models` 从 Cline 的 `recommended-models` 里取 **`free` 那一组**，整理成 OpenAI 的 `{"object":"list","data":[...]}` 返回，其余分组（`recommended` / `clinePass` / `clineCloud`）不列。

```json
{
  "object": "list",
  "data": [
    {"id": "cline-free/deepseek-v4.1-flash", "object": "model", "created": 1790645940, "owned_by": "cline-free"}
  ]
}
```

- 列表**每 10 分钟最多取一次**，之后直接吃缓存；刷新失败时**退回上一份好数据**（客户端常拿它做启动探测，不该因为上游抖一下就一个模型都看不到）。一份都没有才返回 502。
- 这个请求**不带任何 API key**，也不动池子状态 —— 它没有额度可花，也不该影响某个模型在用哪个 key。
- 同样需要 `client_token`（和别的路径一样），只在 `GET`/`HEAD` 上有效。

## 运行

```bash
cp config.example.json config.json && $EDITOR config.json
go build -o cline-api . && ./cline-api
```

或者用发布好的镜像（`docker-compose.yml` 直接拉 ghcr，不本地构建）：

```bash
docker compose up -d          # 想固定版本就改 image: 为 ghcr.io/myflv/cline-api:v0.1.0
```

调用者 `base_url` 指到 `http://<host>:8787/v1`。**Authorization 里填什么都可以** —— 代理会丢掉它，换成池子里的 key。

## 配置

```json
{
  "listen": ":8787",
  "upstream": "https://api.cline.bot/api/v1",
  "client_token": "change-me",
  "cooldown": "60s",
  "cooldown_max": "30m",
  "max_attempts": 3,
  "keys": ["sk-...", "sk-..."]
}
```

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `listen` | `:8787` | 环境变量 `PORT` 可覆盖 |
| `upstream` | `https://api.cline.bot/api/v1` | API 基址，不带 `/chat/completions` |
| `client_token` | 空 | 调用者必须出示的 token；留空则不校验（会打警告） |
| `cooldown` | `60s` | 首次 429 冷却 |
| `cooldown_max` | `30m` | 翻倍上限；同时也是衰减窗口（见上） |
| `max_attempts` | `3` | 单次请求最多用掉几个 key |
| `keys` | 必填 | key 列表 |

时长写 `"90s"` / `"30m"` / `"1h"`，也可以直接写秒数 `90`。

命令行：`-addr` 覆盖监听地址，`-config` 指定配置文件，`-v` 打印每条请求用了哪个 key。

容器里可以用环境变量覆盖文件（env 优先）：`CLINE_KEYS`（逗号分隔）对应 `keys`，`CLIENT_TOKEN` 对应 `client_token`，`PORT` 对应 `listen` 的默认值。只给 `CLINE_KEYS` 而配置文件不存在时，程序按默认值启动——这样镜像里不必放任何密钥。

## 端点

| 端点 | 说明 |
| --- | --- |
| `POST /v1/chat/completions` | 唯一被转发的路径，只接受 `stream: true` |
| `GET /v1/models` | 免费模型列表，来自 Cline 的 `free` 分组 |
| `GET /healthz` | 存活探针，给负载均衡用 |

除 `/healthz` 外都要 token：不带 token 一律 401，带 token 走错路径才 404。

排查「请求为什么落到这个 key」，开 `-v` 看日志：

```
model=cline-free/deepseek-v4.1-flash key=0 429, parked for 1m0s
model=cline-free/deepseek-v4.1-flash key=1 ok
```

`parked for 1m0s` 就是这条的退避等级：第二次是 2m，第三次 4m，以此类推到 `cooldown_max`。不开 `-v` 时只打 429，不打成功请求。

## 边界

- `client_token` 留空时，**任何能访问到这个端口的人都能花你的额度**。默认监听 `:8787`，暴露到公网前务必设上，或者只绑 `127.0.0.1`。
- `config.json` 里有明文 key，别提交进版本库（`.gitignore` 已排除）。
- **非流式请求不可用**，见上：这是上游的能力限制，不是配置项。
- 401/403 不做特殊处理，原样透传：一个失效的 key 会继续占着它负责的模型，直到这个模型的下一次请求拿到 401 —— 但 401 不触发换号，所以它会一直卡在那里直到你重启或换掉这个 key。
- 状态在内存里，重启后所有模型回到 key 0。
- 模型名取自请求体（超过 256 字节直接 400），因为它要当两张表的 key。冷却表里过期超过 `cooldown_max` 的条目由后台每分钟清一次（不占请求路径），回收内存；**游标表（每个用过的模型名一条）故意不清** —— 删掉它等于把一个健康的模型悄悄挪回 key 0，正是这套设计要避免的那次切换。所以内存会随「一共用过多少个不同模型名」增长，这是**唯一一处没有上限的地方**，也是务必设 `client_token` 的另一个理由。
- 请求体上限 64MB。
