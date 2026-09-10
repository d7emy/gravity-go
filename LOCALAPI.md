# gravity-go Local API — Complete Documentation

> **Base URL (default):** `http://127.0.0.1:8964`  
> Bind address and port are configurable via `GRAVITY_HOST` / `GRAVITY_PORT` — see [Configuration](#configuration).  
> All JSON endpoints accept and return `Content-Type: application/json` unless noted. No authentication is required on loopback; the proxy is unauthenticated by design and must not be exposed to the network without a tunnel.

This document describes **every HTTP route** served by `internal/server/server.go:39` and its handlers. It is the contract for any `Anthropic SDK`, `OpenAI SDK`, `curl`, or dashboard client talking to the local proxy.

---

## Table of Contents

1. [Quick Start](#quick-start)
2. [General Behavior](#general-behavior)
3. [Anthropic-Compatible Messages](#anthropic-compatible-messages)
4. [OpenAI-Compatible Chat Completions](#openai-compatible-chat-completions)
5. [Model Listings](#model-listings)
6. [Grounded Web Search](#grounded-web-search)
7. [Health](#health)
8. [Auth & IDE](#auth--ide)
9. [Accounts](#accounts)
10. [Quota / Usage / Settings / Logs](#quota--usage--settings--logs)
11. [Updates, Bundles & Misc](#updates-bundles--misc)
12. [Streaming, Keep-Alive & SSE](#streaming-keep-alive--sse)
13. [Rate Limiting & Account Rotation](#rate-limiting--account-rotation)
14. [Validation Limits](#validation-limits)
15. [Error Handling](#error-handling)
16. [CORS](#cors)
17. [Configuration](#configuration)
18. [Dashboard & Static Assets](#dashboard--static-assets)
19. [curl Cookbook](#curl-cookbook)

---

## Quick Start

```bash
# start the proxy (default port 8964)
go run .
# or
./gravity-go.exe start -p 8964 -v

# health check
curl http://127.0.0.1:8964/health

# list models (whitelisted)
curl http://127.0.0.1:8964/v1/models

# anthropic non-streaming call
curl http://127.0.0.1:8964/v1/messages \
  -H 'content-type: application/json' \
  -d '{"model":"gemini-3.7-flash-high","max_tokens":256,"messages":[{"role":"user","content":"Write a haiku about Go."}]}'

# openai non-streaming call
curl http://127.0.0.1:8964/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"gpt-4","messages":[{"role":"user","content":"Hello!"}]}'

# grounded search
curl 'http://127.0.0.1:8964/search?q=latest%20Go%20release'
```

Point any **Anthropic-compatible** client at `http://127.0.0.1:8964` with any dummy API key, or any **OpenAI-compatible** client at `http://127.0.0.1:8964/v1` with `apiKey: "any"`.

---

## General Behavior

| Property | Detail |
|----------|--------|
| **Body limit** | `64 MiB` on `/v1/messages` and `/v1/chat/completions`; `1 MiB` on `/search` POST and auth routes. |
| **Middleware** | `internal/server/server.go:264` sets CORS, wraps context, records status, logs only `>=400`. |
| **OPTIONS** | Any path with `Origin` returns `204` with CORS headers; no auth. |
| **Unknown path** | `GET /` redirects `302 → /quota`; every other unmatched path returns `404 {"error":{"type":"not_found","message":"Not found"}}` — `internal/server/server.go:178`. |
| **Removed surfaces** | `/routing*`, `/remote*`, `/tunnel/*` are gone and return `404`. |
| **Provider coverage** | `antigravity` only. `/auth/codex/status` & `/auth/copilot/status` return `200 {"success":false,"status":"error","message":"The <provider> provider is not available in this build."}`. |

---

## Anthropic-Compatible Messages

**Routes** — all three are identical, for client compatibility (`internal/server/server.go:91`):

```
POST /v1/messages
POST /v1beta/messages
POST /messages
```

Proxied to upstream `POST {baseURL}/v1internal:streamGenerateContent?alt=sse` where `baseURL` cycles through `internal/antigravity/chat.go:22` (`daily-cloudcode-pa`, `daily-cloudcode-pa.sandbox`, `cloudcode-pa`).

### Request

`internal/server/messages.go:20` — type `anthropicRequest`:

```jsonc
{
  "model": "gemini-3.7-flash-high",     // string, required, max 256 chars
  "messages": [                         // array, required, 1..1000 entries
    {
      "role": "user" | "assistant",     // required
      "content": "hello"                // string OR array of blocks — see below
                // OR [
                //   {"type":"text","text":"hello"},
                //   {"type":"image","source":{"type":"base64","media_type":"image/png","data":"..."}},
                //   {"type":"tool_use","id":"toolu_...","name":"my_tool","input":{...}},
                //   {"type":"tool_result","tool_use_id":"toolu_...","content":"result" | [{"type":"text","text":"..."}],"is_error":false}
                // ]
    }
  ],
  "max_tokens": 1024,                   // int, optional, >=0, <=1_000_000 (clamped to 64000 upstream)
  "system": "You are helpful."          // string OR [{"type":"text","text":"..."}] — optional
           // extracted by internal/server/messages.go:42 (string or block list joined with \n\n)
  ,
  "stream": false,                      // bool, default false
  "temperature": 0.7,                   // *float64, 0..2
  "top_p": 0.9,                         // *float64, finite
  "top_k": 40,                          // *float64, finite
  "tools": [                            // optional, max 100
    {"name":"get_weather","description":"...","input_schema":{"type":"object","properties":{...}}}
  ],
  "tool_choice": {"type":"auto"|"any"|"tool"|"none","name":"..."}, // optional
  "thinking": {"type":"enabled","budget_tokens": 8192} // optional; budget>0 mapped to thinkingBudget
}
```

**Notes**

* `system` can be a bare string or an array of `{text}` blocks. Both are flattened by `extractSystemPrompt` and sent as `systemInstruction` upstream (`internal/antigravity/translate.go:295`). If absent, the upstream receives the built-in `antigravityIdentity` persona.
* Tool names are sanitized to match upstream charset `^[A-Za-z_][A-Za-z0-9_.:-]{0,63}`. Invalid chars → `_`, leading digit → prepended `_`, truncated to 64 chars (`internal/antigravity/translate.go:39`). Original name is restored on response.
* Thinking: if `thinking.budget_tokens > 0`, it becomes `generationConfig.thinkingConfig.thinkingBudget` and is clamped to `maxOutputTokens - 1`. Otherwise, for `gemini*` upstream models, suffix `-low|medium|high` in the **original** model id selects `thinkingLevel: low|medium|high`; `gemini-pro-agent` defaults to `high` when no suffix is present (`internal/antigravity/translate.go:416`).

### Non-Streaming Response

`internal/server/messages.go:184`:

```json
{
  "id": "msg_...",
  "type": "message",
  "role": "assistant",
  "content": [
    {"type":"thinking","thinking":"...","signature":""},   // present only if reasoning was produced
    {"type":"text","text":"Hello!"},
    {"type":"tool_use","id":"toolu_...","name":"my_tool","input":{...}}
  ],
  "model": "gemini-3.7-flash-high",
  "stop_reason": "end_turn" | "tool_use" | "max_tokens",
  "stop_sequence": null,
  "usage": {"input_tokens": 12, "output_tokens": 34}
}
```

`id` is `msg_` + 24 hex chars (`internal/antigravity/translate.go:83`). `stop_reason` maps from upstream tool-use detection; token counts come from the last chunk's `usageMetadata` (`internal/antigravity/response.go:118`).

### Streaming Response (SSE)

**Headers** (`internal/server/messages.go:274`):

```
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
X-Accel-Buffering: no
```

Events are **Anthropic SSE** frames. Upstream chunks are translated by `internal/antigravity/stream.go` (translator) and forwarded via `CreateChatCompletionStream` (`internal/antigravity/chat.go:705`).

**Anthropic SSE event sequence:**

```
event: message_start
data: {"type":"message_start","message":{"id":"msg_...","type":"message","role":"assistant","model":"...","content":[],"usage":{"input_tokens":...}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

# thinking deltas (if any)
event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"..."}}

# tool_use deltas
event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_...","name":"my_tool"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

# keep-alive comments every 15s during silence (see below)
: ping
```

On failure **after headers committed**, an in-band error is sent:

```
event: error
data: {"type":"error","error":{"type":"api_error","message":"..."}}
```

### Example — Anthropic

**Non-streaming:**

```bash
curl -s http://127.0.0.1:8964/v1/messages \
  -H 'content-type: application/json' \
  -d '{
    "model":"gemini-3.7-flash-high",
    "max_tokens": 256,
    "system":"You are a concise assistant.",
    "messages":[{"role":"user","content":"Explain SSE in one sentence."}],
    "temperature": 0.7
  }' | jq .
```

**Streaming:**

```bash
curl -N http://127.0.0.1:8964/v1/messages \
  -H 'content-type: application/json' \
  -H 'accept: text/event-stream' \
  -d '{
    "model":"gemini-3.7-flash-high",
    "stream": true,
    "messages":[{"role":"user","content":"Stream a story in 3 chunks."}]
  }'
```

**Tool use:**

```bash
curl -s http://127.0.0.1:8964/v1/messages \
  -H 'content-type: application/json' \
  -d '{
    "model":"gemini-3.7-flash-high",
    "max_tokens": 1024,
    "messages":[{"role":"user","content":"What is the weather in Paris?"}],
    "tools":[{"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
    "tool_choice":{"type":"auto"}
  }' | jq .
```

**Image (base64):**

```json
{
  "model": "gemini-3.7-flash-high",
  "messages": [{
    "role": "user",
    "content": [
      {"type":"text","text":"Describe this image"},
      {"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0..."}}
    ]
  }]
}
```

**SDK — Anthropic JS/TS:**

```ts
import Anthropic from "@anthropic-ai/sdk";
const client = new Anthropic({
  apiKey: "any", // local proxy ignores it
  baseURL: "http://127.0.0.1:8964",
});
const msg = await client.messages.create({
  model: "gemini-3.7-flash-high",
  max_tokens: 512,
  messages: [{ role: "user", content: "Hello!" }],
});
```

---

## OpenAI-Compatible Chat Completions

**Routes** (`internal/server/server.go:96`):

```
POST /v1/chat/completions
POST /chat/completions
```

### Request

`internal/server/openai.go:19` — type `openAIRequest`:

```jsonc
{
  "model": "gpt-4" | "claude-sonnet-4-5" | "gemini-3.7-flash-high" | "...", // required, mapped via MapOpenAIModel
  "messages": [                       // required, 1..1000
    {"role":"system","content":"You are helpful."},         // extracted to systemInstruction; supports string OR [{type:"text",text:"..."}]
    {"role":"developer","content":"..."},                   // also extracted as system (same as system)
    {"role":"user","content":"hi"},                         // string OR [{type:"text",text:"..."} | {type:"image_url",image_url:{url:"..."}}]
    {"role":"assistant","content":"hello","tool_calls":[{"id":"call_1","type":"function","function":{"name":"my_tool","arguments":"{\"city\":\"Paris\"}"}}]},
    {"role":"tool","tool_call_id":"call_1","content":"sunny, 21C"}
  ],
  "max_tokens": 1024,                 // *int, optional, >0, max 1_000_000
  "temperature": 0.7,                 // *float64, 0..2
  "top_p": 0.9,                       // *float64, finite
  "stream": false,                    // *bool, default false (nil => non-streaming)
  "tools": [                          // optional, max 100
    {"type":"function","function":{"name":"get_weather","description":"...","parameters":{"type":"object","properties":{...}}}}
  ],
  "tool_choice": "auto" | "none" | "required" | {"type":"function","function":{"name":"..."}}, // optional
  "reasoning_effort": "low"|"medium"|"high",   // optional: maps to thinkingBudget 2048/8192/24576
  "reasoning": {"effort":"low"|"medium"|"high"}, // alternative form for reasoning_effort
  "stream_options": {"include_usage": true}    // optional: final usage chunk when streaming
}
```

**Translation notes** (`internal/server/openai.go:62`):

* `system` and `developer` roles are **not** sent as conversations turns — they are joined with `\n\n` and sent as `systemInstruction` (`extractOpenAISystemPrompt`, `internal/server/openai.go:149`). This preserves agent authority.
* `content` may be string or `[{type:"text",text:"..."}, {type:"image_url",image_url:{url:"data:image/...;base64,..."}}]`. `data:` URLs become upstream `inlineData`; remote `https://` URLs degrade to text `[image: https://...]` because upstream only accepts inline data.
* `assistant` turns with `tool_calls` become `tool_use` blocks (args JSON decoded; on parse failure `{}`).
* `tool` turns become `tool_result` on a `user` turn: `Content` is JSON-marshaled string (`internal/server/openai.go:192`).
* `tool_choice` mapping: `"auto"→auto`, `"none"→none`, `"required"→any`, `{"type":"function","function":{"name":"x"}} → {"type":"tool","name":"x"}`.
* `reasoning_effort` / `reasoning.effort` → `ThinkingBudget` (`low=2048`, `medium=8192`, `high=24576`) — `internal/server/openai.go:265`.
* Model alias mapping `internal/antigravity/models.go:99`: `gpt-4→claude-sonnet-4-5`, `gpt-4o→claude-sonnet-4-5`, `gpt-4-turbo→claude-sonnet-4-5`, `gpt-3.5-turbo→gemini-2.0-flash-exp`, `o1→claude-sonnet-4-5-thinking`, `o1-mini→gemini-2.0-flash-exp`. Unknown ids pass through, then mapped again via `UpstreamModelName` (`internal/antigravity/models.go:55`).
* `max_tokens` clamps to `64000` upstream; unchecked values >0 falling inside limit are sent as `generationConfig.maxOutputTokens` (`internal/antigravity/translate.go:329`).

### Non-Streaming Response

`internal/server/openai.go:396` returns OpenAI shape:

```json
{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "created": 1714600000,
  "model": "gpt-4",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "Hello!",
      "tool_calls": [   // only if tool_use produced
        {"id":"call_...","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}
      ],
      "reasoning_content": "...",  // present if reasoning emitted; duplicated as "reasoning"
      "reasoning": "..."
    },
    "finish_reason": "stop" | "tool_calls" | "length"
  }],
  "usage": {"prompt_tokens": 12, "completion_tokens": 34, "total_tokens": 46}
}
```

`finish_reason` mapping `internal/server/openai.go:414`: `tool_use→tool_calls`, `max_tokens→length`, others → `stop`. Reasoning is set only when `result.Reasoning != ""` and is duplicated for DeepSeek vs OpenRouter clients.

### Streaming Response (OpenAI SSE)

**Headers:** same as Anthropic path, keep-alive included (`internal/server/openai.go:595`).

Chunks are `data: {json}\n\n` with final `data: [DONE]\n\n`. State machine `internal/server/openai.go:430` consumes Anthropic SSE frames and re-emits OpenAI chunks:

**OpenAI chunk shape:**

```json
{
  "id": "chatcmpl-...",
  "object": "chat.completion.chunk",
  "created": 1714600000,
  "model": "gpt-4",
  "choices": [{
    "index": 0,
    "delta": {
      "role": "assistant",             // first chunk only
      "content": "Hello",              // text deltas
      "reasoning_content": "...",      // thinking deltas (duplicated as "reasoning")
      "reasoning": "...",
      "tool_calls": [                  // terminal tool_calls (assembled across input_json_delta)
        {"index":0,"id":"call_...","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}
      ]
    },
    "finish_reason": null | "stop" | "tool_calls" | "length"
  }]
}
```

Behavior:

* First `message_start` emits `delta.role = "assistant"` exactly once.
* `content_block_delta` with `text_delta` → `delta.content`.
* `thinking_delta` → `delta.reasoning_content` + `delta.reasoning`.
* `input_json_delta` accumulates per `content_block_start` tool id into `pendingToolCall`; on `message_delta` with any accumulated calls, a single chunk carries all `tool_calls` with `finish_reason: tool_calls`, then a following chunk carries the stop reason.
* `message_delta` `stop_reason` is mapped via `mapStopReason`; if no tool calls, a final empty-delta chunk carries it.
* If `stream_options.include_usage: true`, a final chunk **before** `[DONE]` carries `choices:[]` + `usage:{prompt_tokens, completion_tokens, total_tokens}` (`internal/server/openai.go:641`).

On upstream error after headers, an error data frame then `[DONE]` is emitted; in-band error carries `{"error":{...}}`.

### Example — OpenAI

**Non-streaming:**

```bash
curl -s http://127.0.0.1:8964/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{
    "model":"gpt-4",
    "messages":[
      {"role":"system","content":"You are concise."},
      {"role":"user","content":"What is Go?"}
    ],
    "temperature": 0.5
  }' | jq .
```

**Streaming:**

```bash
curl -N http://127.0.0.1:8964/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{
    "model":"gpt-4",
    "messages":[{"role":"user","content":"Stream a poem"}],
    "stream": true
  }'
```

**Tool calling:**

```bash
curl -s http://127.0.0.1:8964/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{
    "model":"gpt-4",
    "messages":[{"role":"user","content":"Weather in Berlin?"}],
    "tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],
    "tool_choice":"auto"
  }' | jq .
```

**Reasoning effort + usage in stream:**

```bash
curl -N http://127.0.0.1:8964/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{
    "model":"gpt-4",
    "messages":[{"role":"user","content":"Think step by step about prime numbers"}],
    "reasoning_effort":"high",
    "stream": true,
    "stream_options":{"include_usage": true}
  }'
```

**SDK — OpenAI JS:**

```ts
import OpenAI from "openai";
const client = new OpenAI({
  apiKey: "any",
  baseURL: "http://127.0.0.1:8964/v1",
});
const r = await client.chat.completions.create({
  model: "gpt-4",
  messages: [{ role: "user", content: "Hello!" }],
});
```

**Image:**

```json
{
  "model": "gpt-4",
  "messages": [{
    "role": "user",
    "content": [
      {"type":"text","text":"Describe this image"},
      {"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0..."}}
    ]
  }]
}
```

---

## Model Listings

```
GET /v1/models
GET /v1beta/models
GET /models
```

Handler `internal/server/server.go:447`. Returns both Anthropic (`type`) and OpenAI (`object`) compat fields.

**Response:**

```json
{
  "object": "list",
  "data": [
    {
      "id": "gemini-3.7-flash-high",
      "type": "model",
      "object": "model",
      "created_at": "2026-09-02T00:00:00Z",
      "created": 1725235200,
      "owned_by": "antigravity",
      "display_name": "Gemini 3.7 Flash (High)"
    }
  ],
  "has_more": false,
  "first_id": "gemini-3.7-flash-high",
  "last_id": "claude-opus-4-6-thinking"
}
```

**Whitelist** — only visible models appear (`internal/antigravity/models.go:31`):

| `id` | `display_name` |
|------|----------------|
| `gemini-3.7-flash-high` | Gemini 3.7 Flash (High) |
| `gemini-3.1-pro-high` | Gemini 3.1 Pro (High) |
| `claude-opus-4-6-thinking` | Claude Opus 4.6 (Thinking) |

The full catalogue (`internal/antigravity/models.go:10`) contains all callable ids; any callable id can be used in chat requests even if not listed (pass-through via `UpstreamModelName`). Visible filtering is via `FilterVisibleModels`.

**Model translation** — all known aliases collapse onto stable upstream ids (`internal/antigravity/models.go:55`):

```
claude-sonnet-4-5 → claude-sonnet-4-5
claude-sonnet-4-5-thinking → claude-sonnet-4-5-thinking
claude-opus-4-6 → claude-opus-4-6-thinking
claude-sonnet-4.5 → claude-sonnet-4-5
gemini-3-pro-high → gemini-3.1-pro-low
gemini-3.1-pro-high → gemini-pro-agent
gemini-3.7-flash → gemini-3.7-flash-tiered
gemini-3.7-flash-high → gemini-3.7-flash-tiered
gpt-oss-120b → gpt-oss-120b-medium
... (see file for full table)
```

**Example:**

```bash
curl -s http://127.0.0.1:8964/v1/models | jq .data[].id
curl -s http://127.0.0.1:8964/models | jq .
```

---

## Grounded Web Search

```
GET  /search
POST /search
```

Proxied via `internal/antigravity/search.go:262` → `POST {baseURL}/v1internal:generateContent` with `tools: [{googleSearch:{}}]`, model default `gemini-3.7-flash-tiered`, `thinkingBudget: 32768`, `maxOutputTokens: 64000`. Handler `internal/server/search.go:52`.

### GET /search

**Query parameters:**

| Param | Required | Description |
|-------|----------|-------------|
| `q` or `query` | ✅ | Search query, 1..2000 chars, trimmed. `q` preferred; `query` alias. |
| `model` | ❌ | Search model override; default `gemini-3.7-flash-tiered` or `GRAVITY_SEARCH_MODEL` / `ANTI_API_SEARCH_MODEL`. |
| `format` | ❌ | `json` (default) or `text`. `text` returns `text/plain`. |
| `token` | ❌ | Search token if `GRAVITY_SEARCH_TOKEN` is set (see Authorization). |

### POST /search

Query `?token=` and header `Authorization` still honored. JSON body:

```json
{
  "q": "latest Go release",   // or "query"
  "model": "gemini-3.7-flash-high",
  "format": "json" | "text"
}
```

Missing `q`/`query` → `400 {"error":{"type":"invalid_request_error","message":"Missing required field: q"}}`.

### Authorization

If `GRAVITY_SEARCH_TOKEN` (or `ANTI_API_SEARCH_TOKEN`) is set, the request must provide it via **one** of (`internal/server/search.go:19`):

* `?token=…` query param
* `Authorization: Bearer <token>` (case-insensitive `Bearer`) or bare `Authorization: <token>`

Otherwise `401 {"error":{"type":"unauthorized","message":"Invalid or missing search token"}}`. When no env var is set, search is open.

### Response (`format=json`)

`internal/antigravity/search.go:55` — type `SearchResult`:

```json
{
  "query": "latest Go release",
  "answer": "Go 1.22 is the latest...",
  "sources": [
    {"title":"go.dev","url":"https://go.dev/dl/"}
  ],
  "citations": [
    {"text":"Go 1.22 ...","sourceIndexes":[0]}
  ],
  "searched": ["latest go release"],
  "model": "gemini-3.7-flash-tiered",
  "elapsedMs": 1234
}
```

* `sources` are **deduplicated by URL**; index remapping is applied to citations (`internal/antigravity/search.go:159`).
* `citations` link grounded text segments to source indexes; only segments with at least one remapped index are kept.
* `searched` are `groundingMetadata.webSearchQueries`.
* `answer` is concatenated non-thought `parts[].text`.
* `elapsedMs` is server-measured wall time.

### Response (`format=text`)

`text/plain; charset=utf-8`, rendered by `internal/server/search.go:203`:

```
<answer>

Sources:
  [1] go.dev - https://go.dev/dl/
  [2] tip.golang.org - https://tip.golang.org/

Searched: latest go release | query variant 2
```

`(no answer)` when no answer; title falls back to URL hostname when `title == ""`.

### Errors

* `400` — missing/empty query or `>2000` chars.
* `401` — token mismatch when `GRAVITY_SEARCH_TOKEN` is set.
* `405` — method other than GET/POST.
* Upstream non-2xx → mapped status (or `502` if out of range) with `{"error":{"type":"upstream_error","message":..., "provider":"antigravity", "reason":...}}` (`internal/server/search.go:175`).

Success also logs `[time] 200 search•model•N sources•elapsed` via `internal/logx`.

### Examples

```bash
# open search
curl -s 'http://127.0.0.1:8964/search?q=latest%20Go%20release' | jq .
curl -s 'http://127.0.0.1:8964/search?q=Go%20release&format=text'

# token-protected search
export GRAVITY_SEARCH_TOKEN=s3cret
curl -s 'http://127.0.0.1:8964/search?q=hello&token=s3cret' | jq .
curl -s 'http://127.0.0.1:8964/search?q=hello' -H 'Authorization: Bearer s3cret' | jq .

# POST
curl -s http://127.0.0.1:8964/search -H 'content-type: application/json' \
  -d '{"q":"what is antigravity","format":"json"}' | jq .

curl -s 'http://127.0.0.1:8964/search?token=s3cret' -H 'content-type: application/json' \
  -d '{"q":"hello","format":"text"}'
```

---

## Health

```
GET /health
```

Handler `internal/server/server.go:429`:

```json
{
  "status": "ok",
  "authenticated": true
}
```

`authenticated` reflects `appstate.IsAuthenticated()` (OAuth or IDE token present).

```bash
curl -s http://127.0.0.1:8964/health | jq .
```

---

## Auth & IDE

### `GET /auth/status`

`internal/server/auth.go:17` — reports current session:

```json
{
  "authenticated": true,
  "email": "you@gmail.com",  // JSON null when absent (nullableString)
  "name": "Your Name"
}
```

### `GET /auth/accounts`

`internal/server/auth.go:34` — provider-keyed map for dashboard (`internal/authstore`):

```json
{
  "accounts": {
    "antigravity": [
      {"id":"you@gmail.com","email":"you@gmail.com","provider":"antigravity","label":"you@gmail.com","...": "..."}
    ],
    "codex": [],
    "copilot": [],
    "zed": [],
    "kiro": [],
    "grok": []
  }
}
```

Only `antigravity` is populated; others are empty arrays for compat (`internal/server/server_test.go:158`).

### `POST /auth/login`

`internal/server/auth.go:66` — two modes:

**Mode A — empty body → interactive OAuth:**

* Starts `StartOAuthLogin` on `context.WithoutCancel(r.Context())` (detached 5-minute browser flow). Opens browser unless `GRAVITY_OAUTH_NO_OPEN=1`.
* On success, adds account to manager from `appstate.GetAuth()`.

**Mode B — JSON body with tokens → direct registration:**

```json
{
  "accessToken": "ya29....",
  "refreshToken": "1//...",
  "email": "you@gmail.com",
  "name": "Your Name",
  "provider": "antigravity"   // if present, must == "antigravity"
}
```

Other `provider` values → `400 {"success":false,"error":"This build supports the antigravity provider only."}`.

Success (`200`):

```json
{"success": true, "authenticated": true, "provider":"antigravity", "email":"you@gmail.com", "name":"Your Name"}
```

Failure → `400 {"success":false,"error":"..."}`.

```bash
curl -s http://127.0.0.1:8964/auth/status | jq .
curl -s http://127.0.0.1:8964/auth/accounts | jq .

# direct token login
curl -s http://127.0.0.1:8964/auth/login -H 'content-type: application/json' \
  -d '{"accessToken":"ya29...","refreshToken":"1//...","email":"you@gmail.com"}' | jq .

# interactive
curl -s -X POST http://127.0.0.1:8964/auth/login -H 'content-type: application/json' -d '{}' | jq .
```

### `GET /auth/login/pending`

`internal/server/auth.go:382` — dashboard polls this while `POST /auth/login` blocks:

```json
{"active": true, "url":"https://accounts.google.com/o/oauth2/...","browserOpened": true}
```

### `POST /auth/logout`

Clears `appstate` (`antigravity.ClearAuth`):

```json
{"success": true, "authenticated": false}
```

### `GET /auth/diagnostics`

**Loopback-only** (`internal/server/server.go:384` — checks `RemoteAddr` is `127.0.0.1`/`::1`/`localhost`; fallback to `Host` only for `httptest` synthetic `192.0.2.1`). Non-loopback → `403 {"success":false,"error":"Diagnostics is only available from localhost."}`.

On loopback, returns per-account health (`internal/server/auth.go:169`):

```json
{
  "success": true,
  "generatedAt": "2026-09-02T00:00:00Z",
  "accounts": [
    {
      "id": "you@gmail.com",
      "provider": "antigravity",
      "displayName": "you@gmail.com",
      "hasRefreshToken": true,
      "hasProjectId": true,
      "rateLimited": false,
      "expiresAt": 1725235200000,
      "expired": false,
      "expiresInSeconds": 3500
    }
  ],
  "ide": {"loggedIn": true, "email":"...","name":"..."}
}
```

### `GET /auth/ide/status` / `POST /auth/ide/logout`

Proxy to `internal/idedb`. `Status` reports `{loggedIn, email, name, ...}`; `Logout` clears `state.vscdb` and returns `{success, previousEmail}`.

### Unsupported providers

```
GET /auth/codex/status
GET /auth/copilot/status
```

→ `200 {"success":false,"status":"error","message":"The <provider> provider is not available in this build."}`.

### Bundles (removed)

```
GET  /auth/export     → 410 {"success":false,"error":"Credential bundle export/import has been removed."}
POST /auth/import     → 410
GET  /bundle/export   → 410
POST /bundle/import   → 410
```

---

## Accounts

### `POST /accounts/ping`

`internal/server/auth.go:248` — measures round-trip latency via a 1-token completion:

**Request:**

```json
{
  "provider": "antigravity",   // optional, if present must == "antigravity"
  "accountId": "you@gmail.com", // required
  "modelId": "gemini-3.7-flash-high" // optional, default "gemini-3.7-flash-high"
}
```

**Success (`200`):**

```json
{"success": true, "provider":"antigravity","accountId":"you@gmail.com","modelId":"gemini-3.7-flash-high","latencyMs": 842}
```

**Failure — still `200` but `success:false`** (dashboard renders on card):

```json
{
  "success": false,
  "provider":"antigravity",
  "accountId":"you@gmail.com",
  "modelId":"gemini-3.7-flash-high",
  "status": 429,
  "error": "Quota exhausted",
  "reason": "quota_exhausted"
}
```

Missing `accountId` → `400 {"success":false,"error":"provider and accountId are required"}`.

```bash
curl -s http://127.0.0.1:8964/accounts/ping -H 'content-type: application/json' \
  -d '{"accountId":"you@gmail.com","modelId":"gemini-3.7-flash-high"}' | jq .
```

### `DELETE /accounts/{id}`

`internal/server/auth.go:302` — deletes by `id` or email. Tries manager first, then `authstore.Delete` fallback.

* Success: `200 {"success":true,"message":"Account you@gmail.com removed"}`
* Not found: `404 {"success":false,"error":"Account not found"}`

```bash
curl -s -X DELETE http://127.0.0.1:8964/accounts/you@gmail.com | jq .
```

### `POST /accounts/{id}/enabled`

`internal/server/auth.go:336` — **pause/resume** an account without deleting credentials. Paused accounts are excluded from rotation, explicit `accountId` selection, and emergency fallback (`internal/antigravity/chat.go:648`).

**Request:** `{"enabled": true | false}` — field is `*bool`; missing or malformed → `400 {"success":false,"error":"Body must be {\"enabled\": true|false}"}`.

**Response:**

```json
{
  "success": true,
  "enabled": false,
  "accountId": "you@gmail.com",
  "enabledCount": 1,
  "message": "Account you@gmail.com disabled"
}
```

Unknown `id` → `404 {"success":false,"error":"Account not found"}`.

When **all** accounts are paused, any model request returns `503 {"error":{"type":"upstream_error","message":"All accounts are paused. Enable one in the dashboard to resume.","provider":"antigravity"}}` — `internal/antigravity/chat.go:654`.

Re-enabling clears any prior cooldown (`internal/antigravity/accounts.go:499`).

```bash
# pause
curl -s http://127.0.0.1:8964/accounts/you@gmail.com/enabled \
  -H 'content-type: application/json' -d '{"enabled":false}' | jq .

# resume
curl -s http://127.0.0.1:8964/accounts/you@gmail.com/enabled \
  -H 'content-type: application/json' -d '{"enabled":true}' | jq .
```

When the last account is paused, `enabledCount:0` — dashboard should warn.

---

## Quota / Usage / Settings / Logs

### `GET /quota/json`

`internal/server/misc.go:41` — snapshot for dashboard cards. Built by `internal/quotaagg/quotaagg.go:92`.

**Behavior:** Fans out one goroutine per stored account, bounded by a jittered 3..5s window around `fetchTimeout = 4s` (`internal/antigravity.QuotaFetchTimeout`). Each account hits `retrieveUserQuotaSummary` then falls back to `fetchAvailableModels` (→ grouped bars). Uses cached bars on failure; `quota-cache.json` is written to disk for offline starts. Token refresh is delegated to manager (coalesced).

**Response (`internal/quotaagg/quotaagg.go:38`):**

```json
{
  "timestamp": "2026-09-02T00:00:00Z",
  "accounts": [
    {
      "provider": "antigravity",
      "accountId": "you@gmail.com",
      "displayName": "you@gmail.com",
      "enabled": true,
      "bars": [
        {"key":"gemini-weekly","label":"Gemini weekly","percentage": 73, "resetTime":"2026-09-07T00:00:00Z"},
        {"key":"gemini-5h","label":"Gemini 5h","percentage": 100},
        {"key":"3p-weekly","label":"Claude weekly","percentage": 45},
        {"key":"3p-5h","label":"Claude 5h","percentage": 90}
      ],
      "models": [
        {"id":"gemini-3.7-flash-high","label":"Gemini 3.7 Flash (High)"},
        {"id":"gemini-3.1-pro-high","label":"Gemini 3.1 Pro (High)"},
        {"id":"claude-opus-4-6-thinking","label":"Claude Opus 4.6 (Thinking)"}
      ]
    }
  ]
}
```

Bars are either **summary bars** (`BuildQuotaBars` from `internal/antigravity/quota.go:159`) or **model-grouped bars** (`BuildModelBars`: `claude&gpt`, `gpro`, `gflash` — worst-case percentage per group, soonest `resetTime`). Default zeroed bars when both endpoints fail.

```bash
curl -s http://127.0.0.1:8964/quota/json | jq .
```

### `GET /usage`

`internal/server/misc.go:45` — usage report from `internal/usage/usage.go:294`.

**Response:**

```json
{
  "lastUpdated": "2026-09-02T00:00:00Z",
  "today": "2026-09-02",
  "models": [
    {
      "model": "gemini-3.7-flash-tiered",
      "input": 12000,
      "output": 4500,
      "inputCost": 0.01,
      "outputCost": 0.02,
      "cost": 0.03,
      "inputRate": 0.75,
      "outputRate": 3.75,
      "rateBasis": "gemini 3.7 flash",
      "costExact": 0.025875
    }
  ],
  "totalCost": 0.03,
  "totalCostExact": 0.025875,
  "daily": [
    {"date":"2026-09-02","cost":0.03,"costExact":0.025875,"input":12000,"output":4500}
  ],
  "rates": [
    {"family":"gemini 3.7 flash","input":0.75,"output":3.75,"kind":"model","note":"50% promotional discount, from $1.50/$7.50 list"},
    {"family":"claude","input":5.0,"output":25.0,"kind":"family"},
    {"family":"gemini","input":2.0,"output":12.0,"kind":"family"},
    {"family":"gpt","input":1.75,"output":14.0,"kind":"family"}
  ],
  "rateNote": "Estimates only. Antigravity bills against a weekly quota, not per token, so no figure here was actually charged. ..."
}
```

* Rates: model rates (`kind:model`) are real published prices; family rates (`kind:family`) are coarse fallbacks inherited from original `anti-api` (`internal/usage/usage.go:23`). Rates are `USD per million tokens`.
* `cost` is rounded to 2 decimals; `costExact` is unrounded (sub-cent rows would otherwise be `$0.00`).
* `models` sorted by descending `cost`; `daily` holds last 14 days.
* `rateFor` matches `modelRate.Match` as substring of lowercased id, so one entry covers `gemini-3.7-flash-tiered` and `gemini-3.7-flash-high`.

```bash
curl -s http://127.0.0.1:8964/usage | jq .
```

### `POST /usage/reset`

Clears all usage (`internal/usage/usage.go:361`) and immediately writes `usage.json`.

```json
{"success": true}
```

```bash
curl -s -X POST http://127.0.0.1:8964/usage/reset -H 'content-type: application/json' -d '{}' | jq .
```

### `GET /settings` / `POST /settings`

Persisted in `settings.json` via `internal/settings/settings.go:60`.

**GET** returns full `AppSettings`:

```json
{
  "language": "en",          // or "zh-CN" when LANG contains zh
  "preloadRouting": true,
  "autoNgrok": false,
  "autoOpenDashboard": true,
  "autoRefresh": true,
  "autoRestart": false,
  "privacyMode": false,
  "compactLayout": false,
  "trackUsage": true,
  "optimizeQuotaSort": false,
  "captureLogs": false
}
```

**POST** accepts a **partial JSON patch** — only sent keys are merged (`internal/settings/settings.go:81` → `internal/server/server.go:480`). Example:

```bash
curl -s http://127.0.0.1:8964/settings | jq .

curl -s http://127.0.0.1:8964/settings -H 'content-type: application/json' \
  -d '{"captureLogs": true, "optimizeQuotaSort": true}' | jq .

# partial update does NOT reset untouched keys (TestSettingsRoundTrip)
curl -s http://127.0.0.1:8964/settings -H 'content-type: application/json' \
  -d '{"captureLogs": false}' | jq .
```

Invalid JSON → `400 {"error":{"type":"invalid_request_error","message":"Invalid JSON body"}}`.

`captureLogs` toggles `logbuf` capture (see Logs).

### `GET /logs` / `GET /logs/stream`

`internal/server/misc.go:59` & `internal/logbuf/logbuf.go`.

In-memory ring buffer, default `2000` lines (env `GRAVITY_LOG_LINES` / `ANTI_API_LOG_LINES` ≥100). Capture must be enabled via `settings.captureLogs` or log calls are no-op.

**GET /logs?limit=500&since=123**

```json
{
  "entries": [
    {"id": 124, "ts":"2026-09-02T00:00:00.000Z","level":"info","line":"[00:00:00] 200 ..."},
    {"id": 125, "ts":"...","level":"warn","line":"..."}
  ],
  "lastId": 125,
  "maxLines": 2000,
  "enabled": true
}
```

* `limit` defaults to `500`, clamped to `maxLines`.
* `since` (`sinceID`) → only entries with `id > since`. Without `since`, returns last `limit` entries.
* When `enabled:false`, returns `{entries:[], maxLines, enabled:false}`.

**GET /logs/stream?limit=500&since=0** — SSE:

```
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
```

Events:

```
event: log
data: {"id":124,"ts":"...","level":"info","line":"..."}

# when capture disabled:
event: disabled
data: Log capture disabled
```

Replays backlog then streams new `Append` entries via `Subscribe` (channel buffer 256, drops on stall). Unsubscribe on client disconnect.

```bash
curl -s 'http://127.0.0.1:8964/logs?limit=100' | jq .

# stream
curl -N http://127.0.0.1:8964/logs/stream
curl -N 'http://127.0.0.1:8964/logs/stream?since=120'
```

---

## Updates, Bundles & Misc

### `GET /updates/check` / `POST /updates/apply`

Self-update is **not included** in this build. Handlers `internal/server/misc.go:119`:

**GET /updates/check** → `200`:

```json
{
  "success": true,
  "status": "blocked",
  "updateAvailable": false,
  "canApply": false,
  "currentVersion": "1.0.0",
  "latestVersion": "1.0.0",
  "message": "Self-update is not available in this build.",
  "commandHint": "rebuild with: go build -o gravity-go.exe ."
}
```

Required shape — dashboard throws red `Update failed: HTTP 200` if `success` or any key is missing (`internal/server/server_test.go:439`).

**POST /updates/apply** → `200`:

```json
{"success": false, "error": "Self-update is not available in this build. Rebuild with: go build -o gravity-go.exe ."}
```

### Unsupported OpenAI surfaces (`internal/server/server.go:104`)

```
POST /embeddings          → 501 {"error":{"type":"not_supported","message":"Embeddings not supported"}}
POST /v1/embeddings       → 501
POST /responses           → 501 {"error":{"type":"not_supported","message":"Responses API not supported"}}
POST /v1/responses        → 501
POST /v1/images/generations → 501 {"error":{"type":"not_supported","message":"Image generation not supported"}}
```

### Bundle routes (removed) — `410 Gone`

```
GET  /auth/export
POST /auth/import
GET  /bundle/export
POST /bundle/import
```

### Root & Dashboard pages

```
GET /        → 302 Location: /quota
GET /quota   → 200 text/html (embedded quota.html, version-stamped, Cache-Control: no-store)
GET /health  → 200 {"status":"ok","authenticated":bool}
```

All dashboard pages are embedded via `//go:embed all:public` (`internal/server/server.go:30`); the binary is self-contained.

---

## Streaming, Keep-Alive & SSE

Both `POST /v1/messages` with `stream:true` and `POST /v1/chat/completions` with `stream:true` install a **keep-alive** (`internal/server/messages.go:241`):

* Emits SSE comment frame `: ping\n\n` jittered around **15s** (`keepAliveInterval` base ±1/8, mockable in tests) until the handler returns.
* Comment frames are ignored by SSE parsers but keep idle connections alive through proxies and agent clients during long `thinking` silence.
* `stop()` cancels and **waits** for goroutine exit before returning, so late pings cannot land after `ResponseWriter` is finished.

**Upstream streaming** details (`internal/antigravity/chat.go:395`):

* Header timeout: `30s` time-to-first-byte; idle timeout: `15m` silence abort (via `time.AfterFunc` on `idleTimeout`).
* `sseReader` (`internal/antigravity/chat.go:538`) splits on `\n\n` / `\r\n\r\n` with overlap `3` to handle separator straddling reads; scans only newly-appended bytes (O(n) not O(n²)).
* Upstream errors after first byte are marked `StreamingStarted: true` so they cannot be retried.

**In-band SSE error** (headers already sent) differs by protocol:

* Anthropic: `event: error\ndata: {"type":"error","error":{"type":"api_error","message":"..."}}`
* OpenAI: `data: {"error":{"message":"...","type":"api_error"}}` then `data: [DONE]`.

---

## Rate Limiting & Account Rotation

### Global spacing

`internal/server/ratelimit.go:21` — `globalLimiter` enforces `GRAVITY_MIN_REQUEST_INTERVAL_MS` / `ANTI_API_MIN_REQUEST_INTERVAL_MS` default **250 ms** between outbound calls. Request slots are reserved before unlock so concurrent callers stack.

### Per-account concurrency & spacing

`internal/antigravity/accounts.go:101`:

* `GRAVITY_ACCOUNT_CONCURRENCY` / `ANTI_API_ACCOUNT_CONCURRENCY` default `1`, clamped `1..8`. `1` fully serializes an account (protects against per-credential 429s). Higher allows parallel tool calls but raises 429 risk.
* `GRAVITY_ACCOUNT_INTERVAL_MS` default `1000 ms` minimum between two calls on same account. Reserved **including wait** (`now + sleepMs`) to avoid drift.
* `GRAVITY_ACCOUNT_LOCK_WAIT_TIMEOUT_MS` default `45s`; on timeout, proceeds without gate (avoids wedging).

Enforced via `Accounts.AcquireLock` (`internal/antigravity/accounts.go:761`) — a buffered channel semaphore + spacing timer + `inFlight` counter.

### Rotation

`internal/antigravity/accounts.go:935` — accounts used **one at a time in stored order** (`accounts.json` order; queue built deterministically via sorted fallback for map-randomness). First account serves every request until `429 quota_exhausted` triggers cooldown + demotion (`MoveToEndOfQueue`). Rotation is **not quota-aware** (cached quota percentages are not per-request accurate).

* `401` → single token refresh then rotate if multiple accounts.
* `429` logic `internal/antigravity/chat.go:199`:
  * Parse `Retry-After` / body `retryDelay`; classify as `quota_exhausted` vs transient via `isQuotaExhaustedErrorText` (`internal/antigravity/response.go:373`) — `QUOTA_EXHAUSTED` detail, `quota`+`reset`, `quota_exhausted`.
  * Transient (not exhausted): up to `2` retries on same account with increasing wait (cap `4s`), holding rate limit during wait (`HoldRateLimit`).
  * Exhausted: `MarkRateLimitedFromError` (duration from server delay + 500ms, else `10s`; fallback escalates `1m→5m→30m→2h` by failure count), `MoveToEndOfQueue`, transparent retry on `NextExcluding` (naming account to avoid, not `forceRotate` which would skip newly-promoted head).
  * Non-quota 429 out of retries: cooldown `max(delay, 8s)` then rotate; if rotated out, `Retryable:true`.
* `403`/`408`/`404`/`5xx` → try next `baseURL` (`shouldTryNextEndpoint`); `429` is **not** endpoint-retried (account-specific).
* When all accounts are rate-limited:
  * If min wait ≤ `2s` → clear only short cooldowns (`≤2s`) and let best through (preserves long `quota_exhausted` backoffs).
  * Else → `log Warn` and return `nil` → caller's error path surfaces `429`.

### All-paused guard

`internal/antigravity/chat.go:648`: if `Count>0 && EnabledCount==0` → `503 UpstreamError("All accounts are paused. Enable one...")` — prevents fallback to process-level token that belongs to a paused account.

### Endpoint-level 429 retry across baseURLs + rotation

Non-streaming `sendRequestBuffered` budgets `maxRetryAttempts + (Count-1)` + `maxNonQuota429Retries`; streaming `sendRequestStreaming` uses `maxNonQuota429Retries+1+rotationBudget`. Prefers highest-priority status when every endpoint fails (`retryablePriority`: `529:70`, `5xx:60`, `429:50`, `403:45`, etc.).

---

## Validation Limits

`internal/server/validation.go:9`:

| Field | Ceiling | Error message |
|-------|---------|---------------|
| `model` length | `256` chars | `Model name too long (max 256 characters)` |
| `messages` length | `1000` | `Too many messages (max 1000)` |
| `tools` length | `100` | `Too many tools (max 100)` |
| `max_tokens` | `1_000_000` (upstream clamped to `64000`) | `max_tokens too large (max 1000000)` |
| `temperature` | `0..2`, finite | `temperature must be a number between 0 and 2` |
| `top_p` | finite | `top_p must be a finite number` |
| `top_k` | finite | `top_k must be a finite number` |

Specific checks per protocol:

* Anthropic `internal/server/messages.go:65`: `model` required, `messages` non-nil, non-empty, each `role` non-empty, `max_tokens` ≥0 then validated, tools length check.
* OpenAI `internal/server/openai.go:293`: similar; `max_tokens` checked as `*int > limit`; `tools` length unconditional (even when zero, check is len>100, so empty is fine).

Invalid payloads → `400 {"error":{"type":"invalid_request_error","message":"..."}}` and `X-Log-Reason` header set for log correlation (`internal/server/messages.go:142`).

---

## Error Handling

### Anthropic errors (`internal/server/messages.go:304`)

```json
{"error":{"type":"invalid_request_error","message":"Model is required and must be a string"}}
```

Upstream failures use `writeUpstreamError` (`internal/server/messages.go:311`) → status = `upstream.Status`, body:

```json
{
  "error": {
    "type": "upstream_error",
    "message": "<summarized>",
    "provider": "antigravity",
    "reason": "quota_exhausted",          // if present
    "detail": "<first 800 chars of upstream body>" // for 429 only
  }
}
```

### OpenAI errors (`internal/server/openai.go:660`)

Same upstream mapping; request validation errors:

```json
{"error":{"type":"invalid_request_error","message":"Model is required"}}
```

### Search errors (`internal/server/search.go:175`)

```json
{"error":{"type":"unauthorized","message":"Invalid or missing search token"}}
{"error":{"type":"upstream_error","message":"...","provider":"antigravity","reason":"..."}}
```

### Generic local errors

```json
{"error":{"type":"not_found","message":"Not found"}}
{"error":{"type":"error","message":"..."}}
{"error":{"type":"not_supported","message":"..."}}
{"error":{"type":"api_error","message":"..."}}
```

All `>=400` carry `X-Log-Reason` for server log correlation (`internal/server/server.go:287`).

### Retriable classification

`internal/antigravity/retry.go` and `internal/antigravity/chat.go:56` classify retriable; `apperr.UpstreamError` carries `.Status`, `.Body`, `.RetryAfter`, `.Retryable`, `.StreamingStarted`.

---

## CORS

`internal/server/server.go:322` — `applyCORS`:

* Only allowed when `Origin` hostname is **local** (`isLocalHost`):
  * `localhost`, `127.0.0.1`, `::1`, `[::1]`
  * Any of the machine's own interface IPs (enumerated once via `sync.OnceValue` at `internal/server/server.go:346`; stale additions need restart).
* When allowed:
  ```
  Access-Control-Allow-Origin: <origin>  (echoed)
  Vary: Origin
  Access-Control-Allow-Methods: GET,POST,DELETE,OPTIONS
  Access-Control-Allow-Headers: Content-Type,Authorization,x-api-key,anthropic-version
  ```
* Remote `Origin` → no header → browser blocks.
* `OPTIONS` preflight → `204` with CORS headers, no further handling.

`isLoopbackRequest` (`internal/server/server.go:384`) for `/auth/diagnostics` additionally gates on `RemoteAddr` being loopback to prevent `Host` spoofing from LAN.

---

## Configuration

| Variable | Default | Purpose |
|----------|---------|---------|
| `GRAVITY_DATA_DIR` | `~/.anti-api` | Credentials (`auth.json`, `accounts.json`), `settings.json`, `usage.json`, `quota-cache.json`. `ANTI_API_DATA_DIR` fallback. |
| `GRAVITY_IDE_DB_PATH` | platform default | Override `state.vscdb` path. |
| `GRAVITY_HOST` | `127.0.0.1` | Bind address. Fallback `ANTI_API_HOST`. |
| `GRAVITY_PORT` | `8964` | Listen port; `-p` flag wins over env. |
| `GRAVITY_ACCOUNT_CONCURRENCY` | `1` | In-flight per account (1..8). `ANTI_API_ACCOUNT_CONCURRENCY` fallback. |
| `GRAVITY_ACCOUNT_INTERVAL_MS` | `1000` | Min spacing per account. `-1` unset fallback path uses `1000` default. |
| `GRAVITY_MIN_REQUEST_INTERVAL_MS` | `250` | Global spacing. `ANTI_API_MIN_REQUEST_INTERVAL_MS` fallback. `0` disables. |
| `GRAVITY_ACCOUNT_LOCK_WAIT_TIMEOUT_MS` | `45000` | Gate wait timeout before proceeding without lock. |
| `GRAVITY_INSECURE_TLS` | unset | `1` disables TLS verification (for TLS-inspecting corporate proxies). Verifies by default. |
| `GRAVITY_NO_OPEN` | unset | `1` suppresses dashboard auto-open at startup. `ANTI_API_NO_OPEN` fallback. |
| `GRAVITY_OAUTH_NO_OPEN` | unset | `1` suppresses browser for sign-in (show URL on dashboard instead). |
| `GRAVITY_SEARCH_TOKEN` | unset | Require token on `/search`. `ANTI_API_SEARCH_TOKEN` fallback. |
| `GRAVITY_SEARCH_MODEL` | `gemini-3.7-flash-tiered` | Default search model. `ANTI_API_SEARCH_MODEL` fallback. |
| `GRAVITY_LOG_LINES` | `2000` | Log ring size (≥100 enabled). `ANTI_API_LOG_LINES` fallback. |
| `GRAVITY_LOG_LINES` / `ANTI_API_LOG_LINES` | `2000` | Same as above (logbuf). |
| `GRAVITY_OAUTH_NO_OPEN` | unset | As above. |
| `GRAVITY_IDE_VERSION` | `1.15.8` | Pin the presented IDE version. `ANTIGRAVITY_IDE_VERSION` fallback. |
| `GRAVITY_USER_AGENT` | derived | Full upstream User-Agent override (debugging). `ANTIGRAVITY_USER_AGENT` fallback. |
| `GRAVITY_JITTER` | enabled | `0` disables transport jitter (endpoint shuffle, retry/spacing/keep-alive spreads) for deterministic tests. |

`GRAVITY_HOST` binding: `net.JoinHostPort(host, port)` (`main.go:149`). `ANTI_API_HOST=0.0.0.0` is honored for compat and will bind every interface — see README warning.

TLS verification is ON by default (`internal/antigravity/httpclient.go`); set `GRAVITY_INSECURE_TLS=1` only behind a trusted intercepting proxy.

---

## Dashboard & Static Assets

* `GET /quota` serves embedded `public/quota.html` with version-stamped `/vendor/...?v=<hash>` references (`internal/server/server.go:200`). Page is `Cache-Control: no-store` (never cached; carries asset versions).
* `GET /vendor/{file}` — serves **embedded** FS (`internal/server/server.go:137`). Rejects path traversal (`/`, `\`, `..`). Content types: `.js→application/javascript`, `.css→text/css`, `.woff2→font/woff2`, `.svg→image/svg+xml`, other→`application/octet-stream`.
* Caching: `Cache-Control: no-cache` (must revalidate), `ETag: "<hex 8 bytes of SHA256>"` derived from content hash (`vendorETag`). Honors `If-None-Match` → `304 Not Modified`. Version query `?v=` hash is same as `ETag` (trimmed quotes).
* Every reference in HTML is stamped via `versionVendorRefs` (regex `/vendor/(…)`) (`internal/server/server.go:190`).
* Assets served: `app.css`, `chart.umd.min.js`, `comfortaa.woff2`, etc. — verified by `internal/server/server_test.go:388`.

---

## curl Cookbook

```bash
BASE=http://127.0.0.1:8964

# health & diagnostics
curl -s $BASE/health | jq .
curl -s $BASE/auth/status | jq .
curl -s $BASE/auth/accounts | jq .
curl -s $BASE/auth/diagnostics | jq .   # must be from localhost; else 403
curl -s $BASE/auth/ide/status | jq .

# models
curl -s $BASE/v1/models | jq .
curl -s $BASE/v1beta/models | jq .
curl -s $BASE/models | jq .

# anthropic non-streaming (max 64k upstream, 1M validated)
curl -s $BASE/v1/messages -H 'content-type: application/json' -d '{
  "model":"gemini-3.7-flash-high",
  "max_tokens": 512,
  "messages":[{"role":"user","content":"Say hello"}]
}' | jq .

# anthropic streaming (all 3 prefixes equivalent)
curl -N $BASE/v1/messages -H 'content-type: application/json' -d '{
  "model":"claude-opus-4-6-thinking",
  "stream": true,
  "messages":[{"role":"user","content":"Stream me a story"}]
}'
curl -N $BASE/v1beta/messages -H 'content-type: application/json' -d '{"model":"gemini-3.7-flash-high","stream":true,"messages":[{"role":"user","content":"hi"}]}'
curl -N $BASE/messages -H 'content-type: application/json' -d '{"model":"gemini-3.7-flash-high","stream":true,"messages":[{"role":"user","content":"hi"}]}'

# anthropic tool use + thinking
curl -s $BASE/v1/messages -H 'content-type: application/json' -d '{
  "model":"claude-opus-4-6-thinking",
  "max_tokens": 1024,
  "thinking":{"type":"enabled","budget_tokens": 4096},
  "messages":[{"role":"user","content":"Use the tool to get weather for Paris"}],
  "tools":[{"name":"get_weather","description":"Weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]
}' | jq .

# openai non-streaming
curl -s $BASE/v1/chat/completions -H 'content-type: application/json' -d '{
  "model":"gpt-4",
  "messages":[{"role":"user","content":"Hello!"}]
}' | jq .

# openai streaming with reasoning + include_usage
curl -N $BASE/v1/chat/completions -H 'content-type: application/json' -d '{
  "model":"gpt-4",
  "messages":[{"role":"user","content":"Explain quantum computing briefly"}],
  "stream": true,
  "reasoning_effort":"medium",
  "stream_options":{"include_usage": true}
}'

# openai alternate alias
curl -s $BASE/chat/completions -H 'content-type: application/json' -d '{
  "model":"gpt-4",
  "messages":[{"role":"user","content":"Hello via /chat/completions"}]
}' | jq .

# openai tool round-trip (assistant tool_calls → tool result)
curl -s $BASE/v1/chat/completions -H 'content-type: application/json' -d '{
  "model":"gpt-4",
  "messages":[
    {"role":"user","content":"Weather in Tokyo?"},
    {"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}]},
    {"role":"tool","tool_call_id":"call_1","content":"Sunny 26C"}
  ],
  "tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]
}' | jq .

# search (open)
curl -s "$BASE/search?q=latest%20Go%20release" | jq .
curl -s "$BASE/search?q=latest%20Go%20release&format=text"
curl -s $BASE/search -H 'content-type: application/json' -d '{"q":"hello","format":"json"}' | jq .

# search (protected — requires token)
GRAVITY_SEARCH_TOKEN=secret ./gravity-go &
curl -s "$BASE/search?q=test&token=secret" | jq .
curl -s "$BASE/search?q=test" -H 'Authorization: Bearer secret' | jq .

# quota & usage
curl -s $BASE/quota/json | jq .
curl -s $BASE/usage | jq .
curl -s -X POST $BASE/usage/reset -H 'content-type: application/json' -d '{}' | jq .

# settings
curl -s $BASE/settings | jq .
curl -s $BASE/settings -H 'content-type: application/json' -d '{"captureLogs":true}' | jq .
curl -s $BASE/settings -H 'content-type: application/json' -d '{"optimizeQuotaSort":true, "compactLayout": true}' | jq .

# logs
curl -s "$BASE/logs?limit=100" | jq .
curl -N $BASE/logs/stream

# auth
curl -s -X POST $BASE/auth/login -H 'content-type: application/json' -d '{}' | jq .  # interactive
curl -s http://127.0.0.1:8964/auth/login/pending | jq .
curl -s -X POST $BASE/auth/logout -H 'content-type: application/json' -d '{}' | jq .

# accounts
curl -s $BASE/accounts/ping -H 'content-type: application/json' -d '{"accountId":"you@gmail.com"}' | jq .
curl -s -X DELETE $BASE/accounts/you@gmail.com | jq .
curl -s $BASE/accounts/you@gmail.com/enabled -H 'content-type: application/json' -d '{"enabled":false}' | jq .
curl -s $BASE/accounts/you@gmail.com/enabled -H 'content-type: application/json' -d '{"enabled":true}' | jq .

# updates
curl -s $BASE/updates/check | jq .
curl -s -X POST $BASE/updates/apply -H 'content-type: application/json' -d '{}' | jq .

# vendor (with ETag)
curl -i $BASE/vendor/app.css
curl -s $BASE/vendor/app.css -H 'If-None-Match: "<etag>"' -i   # → 304

# cors (localhost allowed, remote blocked)
curl -s $BASE/health -H 'Origin: http://localhost:3000' -i | grep -i access-control
curl -s $BASE/health -H 'Origin: https://evil.example.com' -i | grep -i access-control  # empty
```

---

## Appendix: File Reference

* Routing & middleware: `internal/server/server.go:39`, `internal/server/server.go:264`
* Anthropic handler & validation: `internal/server/messages.go:20`, `internal/server/validation.go:9`
* OpenAI handler & translation: `internal/server/openai.go:19`, `internal/server/openai.go:149`
* SSE & keep-alive: `internal/server/messages.go:199`, `internal/server/messages.go:239`, `internal/antigravity/chat.go:498`
* Models & mapping: `internal/antigravity/models.go:10`
* Translation to upstream: `internal/antigravity/translate.go:349`, `internal/antigravity/translate.go:39`
* Quota & bars: `internal/antigravity/quota.go:79`, `internal/quotaagg/quotaagg.go:92`
* Usage & rates: `internal/usage/usage.go:23`, `internal/usage/usage.go:294`
* Settings: `internal/settings/settings.go:60`
* Logs: `internal/logbuf/logbuf.go:40`, `internal/server/misc.go:59`
* Search: `internal/server/search.go:52`, `internal/antigravity/search.go:262`
* Accounts & rotation: `internal/antigravity/accounts.go:67`, `internal/antigravity/chat.go:629`
* Rate limit: `internal/server/ratelimit.go:21`, `internal/antigravity/accounts.go:101`
* Assets: `internal/server/server.go:137`, `internal/server/server.go:190`

Generated for `gravity-go` — `internal/server/version.go:3` (`Version = 1.0.0`).

