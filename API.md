# gravity-go API

A local proxy that exposes Antigravity's models behind an **Anthropic-compatible**
and an **OpenAI-compatible** API.

- **Base URL:** `http://127.0.0.1:8964`
- **Authentication:** none. The server binds loopback and any local process may
  call it. Client libraries usually insist on a key — send any non-empty string.
- **Content type:** `application/json`

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8964
export ANTHROPIC_API_KEY=dummy
```

```bash
export OPENAI_BASE_URL=http://127.0.0.1:8964/v1
export OPENAI_API_KEY=dummy
```

## Models

| id | notes |
| --- | --- |
| `gemini-3.8-flash-high` | newest flash, default choice |
| `gemini-3.7-flash-high` | previous flash, still available |
| `gemini-3.1-pro-high` | stronger, emits long thinking blocks |
| `claude-opus-4-6-thinking` | separate weekly quota from the Gemini models |

`GET /v1/models`, `/v1beta/models` and `/models` all return the list.

Client-facing ids are translated to upstream ids internally
(`gemini-3.8-flash-high` → `gemini-3.8-flash-tiered`). Send the id from the table
above; an unmapped id is rejected upstream as `404 NOT_FOUND`.

Note that `fetchAvailableModels` lags the rollout — 3.8 serves normally but does
not appear in that catalogue. A model missing from it is not proof it is
unavailable; probe the id directly.

---

## Anthropic Messages API

`POST /v1/messages` — also served at `/v1beta/messages` and `/messages`.

```bash
curl -X POST http://127.0.0.1:8964/v1/messages \
  -H 'content-type: application/json' \
  -d '{
    "model": "gemini-3.7-flash-high",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

```json
{
  "id": "msg_...",
  "type": "message",
  "role": "assistant",
  "model": "gemini-3.7-flash-high",
  "content": [{"type": "text", "text": "Hello!"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 89, "output_tokens": 46}
}
```

### Request fields

| field | type | notes |
| --- | --- | --- |
| `model` | string | **required**, max 256 chars |
| `messages` | array | **required** |
| `max_tokens` | int | **required**, max 1000000 |
| message count | — | up to 100000 turns; long agentic sessions are fine |
| tool count | — | up to 10000 declarations; stacked MCP servers are fine |
| `system` | string or block array | both forms accepted |
| images | content block | base64 only — see [Images](#images) |
| `stream` | bool | see [Streaming](#streaming) |
| `tools` | array | see [Tool use](#tool-use) |
| `tool_choice` | object | `auto`, `any`, `tool` |
| `thinking` | object | `{"type":"enabled","budget_tokens":N}` |
| `temperature` | 0–2 | accepted, **ignored upstream** — see below |
| `top_p` | number | accepted, **ignored upstream** |
| `top_k` | number | accepted, **ignored upstream** |

Content blocks support text, images and tool results.

### Response

`content` is a list of blocks: `text`, `thinking` (reasoning models), and
`tool_use`. `stop_reason` is `end_turn`, `tool_use`, `stop_sequence` or
`max_tokens`.

---

## OpenAI Chat Completions

`POST /v1/chat/completions` — also served at `/chat/completions`.

```bash
curl -X POST http://127.0.0.1:8964/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{
    "model": "gemini-3.7-flash-high",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

Supports `max_tokens`, `temperature`, `top_p`, `stream`, `tools`, `tool_choice`,
`reasoning_effort` (`low` / `medium` / `high`, also accepted as
`reasoning.effort`), and `stream_options.include_usage` for a final usage-only
chunk before `[DONE]`.

Not implemented, and explicitly rejected rather than silently ignored:
embeddings, the Responses API, and image generation.

---

## Streaming

Set `stream: true`. Both endpoints emit `text/event-stream` and flush every
event, and both send a `: ping` comment frame every 15 seconds while the model is
still thinking, so an idle connection is not dropped by a proxy or client.

**Anthropic** events, in order:

```
message_start → content_block_start → content_block_delta…
→ content_block_stop → message_delta → message_stop
```

**OpenAI** emits `chat.completion.chunk` objects and terminates with
`data: [DONE]`.

---

## Tool use

Standard Anthropic and OpenAI tool schemas both work, including multi-turn
round-trips.

```bash
curl -X POST http://127.0.0.1:8964/v1/messages \
  -H 'content-type: application/json' \
  -d '{
    "model": "gemini-3.7-flash-high",
    "max_tokens": 1024,
    "tools": [{
      "name": "get_weather",
      "description": "Get current weather for a city",
      "input_schema": {
        "type": "object",
        "properties": {"city": {"type": "string"}},
        "required": ["city"]
      }
    }],
    "messages": [{"role": "user", "content": "Weather in Riyadh?"}]
  }'
```

The model replies with a `tool_use` block and `stop_reason: "tool_use"`. Send the
result back as a `tool_result` block in a `user` message, exactly as with the
real Anthropic API.

JSON Schema is cleaned for Gemini automatically: `$ref`/`$defs` are flattened and
unsupported keywords dropped. Your original schema is not mutated, so resending
it each turn is safe.

Gemini requires the `thoughtSignature` that arrived with a tool call to be
replayed on later turns; the proxy stores and reattaches these for you, so
resumed sessions keep working across restarts.

---

## Images

Images must be sent as **base64 data**. The upstream cannot fetch a URL, so
there is nothing to send it when a client passes one.

Anthropic shape:

```json
{"type": "image",
 "source": {"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo..."}}
```

OpenAI shape — a `data:` URL is decoded and inlined:

```json
{"type": "image_url",
 "image_url": {"url": "data:image/png;base64,iVBORw0KGgo..."}}
```

The `data:` URL parsing is deliberately tolerant, because clients spell the same
file many different ways. All of these inline correctly:

```
data:image/jpeg;base64,...              canonical
data:image/jpg;base64,...               not a real media type, but common
data:;base64,...                        no media type at all
DATA:IMAGE/JPEG;BASE64,...              uppercase
data:image/jpeg;charset=utf-8;base64,.. extra parameters
data:image/jpeg;base64,AAA<newline>BBB    wrapped across lines
```

The media type is taken from the file's own signature (JPEG, PNG, GIF, WebP)
rather than from what the client claimed, so a mislabelled or unlabelled image
is still sent correctly. Base64 padding and the URL-safe alphabet are both
accepted.

Verified working end to end at both 180 bytes and a 2 MB payload; the 64 MB body
limit is the practical ceiling.

### If images "don't work"

Anything that is not base64 — a `url` source, an `http(s)` URL in
`image_url`, a source that is missing or empty — cannot be inlined. The image is
replaced by a visible `[image omitted: …]` marker explaining why, and a warning
naming the exact shape is written to the log:

```
[image] url sources are not supported upstream (https://…); send the image as base64
```

Check the dashboard's **logs** tab, or `GET /logs?json=1`, to see which shape your
client sent.

The marker never contains the original value. That matters: pasting an unparsed
data URL back in fed megabytes of base64 to the model as prose, and pasting a
file path fed it the file *name* — from which it confidently answered questions
about a picture it had never seen. A wrong answer is worse than a missing one.

A remote `http(s)` URL is the one exception: it is short, and naming it tells you
exactly which image was skipped.

---

## Web search

`GET /search?q=...` or `POST /search` with `{"query": "..."}`.

```bash
curl 'http://127.0.0.1:8964/search?q=capital+of+Saudi+Arabia'
```

```json
{
  "query": "capital of Saudi Arabia",
  "answer": "The capital of Saudi Arabia is **Riyadh**.",
  "sources": [], "citations": [], "searched": [],
  "model": "gemini-3.7-flash-tiered",
  "elapsedMs": 1976
}
```

Query max 2000 chars. Set `GRAVITY_SEARCH_TOKEN` to require a token, passed as
`Authorization: Bearer <token>` or `?token=`.

---

## Management endpoints

Used by the dashboard; usable directly.

| endpoint | purpose |
| --- | --- |
| `GET /health` | liveness |
| `GET /quota` | dashboard UI |
| `GET /quota/json` | per-account quota, models, and `enabled` state |
| `GET /usage` | token usage, cost estimates and the rate table |
| `POST /usage/reset` | clear usage history |
| `GET /settings`, `POST /settings` | dashboard settings |
| `GET /logs`, `GET /logs/stream` | recent log lines; SSE stream |
| `POST /accounts/{id}/enabled` | pause or resume an account |
| `DELETE /accounts/{id}` | remove an account |
| `POST /accounts/ping` | check an account reaches upstream |
| `POST /auth/login` | start browser OAuth |
| `GET /auth/status`, `GET /auth/accounts` | current auth state |

Pause an account without deleting its credentials:

```bash
curl -X POST http://127.0.0.1:8964/accounts/you@gmail.com/enabled \
  -H 'content-type: application/json' -d '{"enabled":false}'
```

---

## Errors

Anthropic-shaped:

```json
{"error": {"type": "invalid_request_error", "message": "temperature must be a number between 0 and 2"}}
```

| status | meaning |
| --- | --- |
| `400` | invalid request (bad parameter, malformed JSON) |
| `404` | unknown model, or unknown account |
| `429` | quota exhausted or rate limited upstream |
| `503` | every account is paused |

A `429` on one account is handled internally: the account is put on cooldown,
demoted, and the request is retried on the next account within the same call. You
see one successful response, not an error — unless every account is exhausted.

---

## Accounts and rotation

Accounts are used **one at a time, in stored order**. The first serves every
request until exhausted; only then does traffic move to the next, and it stays
there. Rotation is deliberately not quota-aware — cached quota percentages are
not accurate per request.

The order in `accounts.json` is the rotation order, and it is stable across token
refreshes, saves and restarts. Reorder that file to choose which account drains
first.

---

## Sampling parameters do not work

`temperature`, `top_p` and `top_k` are accepted, validated, and forwarded
upstream under Gemini's field names — **and the upstream discards them.**

This was measured, not assumed:

- `temperature: 0` returned four different sentences across four calls. Greedy
  decoding cannot do that.
- `temperature: 0` with `top_k: 1` and `top_p: 0` — the strongest determinism
  forcing available — still varied on every call, on both endpoints.
- `maxOutputTokens`, which travels in the *same* `generationConfig` block, **is**
  honoured (17 output tokens at `max_tokens: 20` versus 295 at `400`). So the
  block is read; the sampling fields specifically are dropped.

The proxy's half is correct and pinned by a test
(`TestSamplingParametersReachTheWire`): the values reach the wire, and are
omitted entirely when unset so the upstream default applies.

**Practical consequence:** do not rely on `temperature: 0` for reproducible
output. There is currently no way to make this endpoint deterministic. If a
future upstream change starts honouring these fields, no client change is needed
— they are already being sent.

### Other fidelity gaps

`stop_reason` is reported as `end_turn` even when output was truncated by
`max_tokens`. Detecting truncation by comparing `usage.output_tokens` against the
requested `max_tokens` is more reliable than trusting `stop_reason`.
