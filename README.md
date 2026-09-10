# gravity-go

> Your OpenCode subscription just hit a rate limit mid-refactor. Antigravity didn't. This is the bridge.

Antigravity's built-in models, exposed as a local Anthropic-compatible (and OpenAI-compatible) API. A Go port of the antigravity provider from `anti-api`, shipping as **one static binary** — no Node, no CDN, no files to deploy next to the exe.

```
        .-~~~-.
      .'  GRAVITY  '.
     /   pulls you   \
    |      in.        |
     \   127.0.0.1  /
      '.  :8964   .'
        '-~~~-'
     (no events were harmed.
      your 429s were.)
```

## The crisis this solves

You already pay for OpenCode. Then:

- a long agentic session gets **rejected outright** (one real 669k-token session did),
- a tool-heavy setup with stacked MCP servers slams into a **100-tool ceiling**,
- a Friday-afternoon refactor dies on a **429** with three files left to go,
- and the meter keeps running while you stare at `retry-after`.

`gravity-go` is the escape hatch: point OpenCode at `http://127.0.0.1:8964` and burn **Antigravity's weekly quota** instead — Gemini + Claude, same agent loop, zero per-token anxiety. When one Google account taps out, traffic **transparently fails over to the next one** inside the same request. You see one good response, not an error.

## Why OpenCode users care

- **It speaks both protocols.** `POST /v1/messages` for Anthropic clients, `POST /v1/chat/completions` for OpenAI clients. Streaming, tool use, images, thinking blocks — all translated to what upstream actually accepts.
- **It's built for stupid-long sessions.** 100,000 messages and 10,000 tools per request (up from 1,000 / 100 in the TypeScript build) because every tool call + result is a message and real agent runs hit four figures fast.
- **It survives quota death.** One account serves everything until Google says `429 quota_exhausted`. Then: cooldown, demote to back of queue, retry on the next account — persisted across restarts. Verified on live traffic: an account drained to 0%, cooled down ~5.3 days to weekly reset, and traffic stayed moved.
- **It's one file.** Dashboard embedded via `go:embed`. Works offline, behind firewalls, on a plane.

Quickest wiring (matches [OpenCode provider docs](https://opencode.ai/docs/providers/) — `baseURL` override pattern):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "anthropic": {
      "options": { "baseURL": "http://127.0.0.1:8964" }
    }
  }
}
```

Or as a dedicated local provider (same shape as the Ollama/LM Studio examples in those docs):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "gravity-go": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "gravity-go (local)",
      "options": { "baseURL": "http://127.0.0.1:8964/v1" },
      "models": {
        "gemini-3.8-flash-high": { "name": "Gemini 3.8 Flash (High)" },
        "gemini-3.7-flash-high": { "name": "Gemini 3.7 Flash (High)" },
        "claude-opus-4-6-thinking": { "name": "Claude Opus 4.6 (Thinking)" }
      }
    }
  }
}
```

Dummy key everywhere (`ANTHROPIC_API_KEY=dummy`). The proxy binds loopback and needs no key.

## Install & run

Requires **Go 1.25.7+**. No other toolchain.

```bash
git clone https://github.com/<you>/gravity-go.git
cd gravity-go

# shortest path — no build step, no arguments:
go run .

# or a standalone binary:
go build -o gravity-go.exe .
./gravity-go.exe
```

That starts the proxy on port **8964**. Running with no arguments is the same as `start`. Dashboard: http://localhost:8964/quota

```bash
gravity-go                         # start proxy (same as start)
gravity-go start [-p PORT] [-v]    # start proxy, default 8964
gravity-go login                   # add a Google account via browser OAuth
gravity-go accounts                # list configured accounts
gravity-go logout-ide              # close Antigravity, clear its stored session
gravity-go version                 # print version
```

## Auth: two paths, tried in order

1. **OAuth** — `gravity-go login` runs Google consent against a loopback callback and saves the token pair to `~/.anti-api/auth.json`. Add several; they rotate automatically when one hits quota.
2. **Local IDE** — no OAuth session? The token is read straight out of Antigravity's `state.vscdb` (SQLite, read-only). Works while the IDE is running.

Credentials live in the same `~/.anti-api` layout the TypeScript build used (`auth.json`, `accounts.json`, `settings.json`, `usage.json`, `quota-cache.json`), so an existing install is picked up with no migration. Override with `GRAVITY_DATA_DIR`.

## What you actually get

**Models** (`GET /v1/models`, `/v1beta/models`, `/models`):

| id | notes |
| --- | --- |
| `gemini-3.8-flash-high` | newest flash, default pick |
| `gemini-3.7-flash-high` | previous flash, still there |
| `gemini-3.1-pro-high` | stronger, long thinking blocks |
| `claude-opus-4-6-thinking` | separate weekly quota from Gemini |

Client ids map to upstream wire ids internally (`gemini-3.8-flash-high` → `gemini-3.8-flash-tiered`). More ids are callable than listed — the listing is a whitelist, not the limit. `fetchAvailableModels` lags rollouts (3.8 serves fine while missing from the catalogue), so probe the id directly.

**Anthropic:** `POST /v1/messages` (also `/v1beta/messages`, `/messages`) — streaming + non-streaming, tool use, base64 images, thinking blocks.

**OpenAI:** `POST /v1/chat/completions` (also `/chat/completions`) — streaming + non-streaming, tool calls, `reasoning_effort` (`low`/`medium`/`high`, or `reasoning.effort`), `stream_options.include_usage`.

**Search:** `GET|POST /search` — grounded web search, returns answer + sources + citations + what was actually searched.

**Dashboard:** `/quota`, `/quota/json`, `/usage`, `/settings`, `/logs`, `/auth/*`, `/accounts/*` — including `POST /accounts/{id}/enabled` to pause/resume without deleting credentials. Fully self-contained: vendored CSS/JS/fonts, no CDN, works offline. Design tokens live in `DESIGN.md`.

Full request/response reference: [API.md](API.md). Every-route contract: [LOCALAPI.md](LOCALAPI.md).

## Rotation, pausing, and the 503 you asked for

- Accounts run **one at a time, in stored order**. First in `accounts.json` drains first. Reorder the file to pick your sacrifice. Deliberately *not* quota-aware — cached quota percentages lie at per-request granularity.
- On `quota_exhausted` 429: cooldown + demote + transparent retry. Caller sees success.
- Dashboard switch (or `curl -X POST .../accounts/you@gmail.com/enabled -d '{"enabled":false}'`) pauses an account. Pause keeps creds, project id, quota history — resume needs no re-login and clears leftover cooldown.
- Paused means paused: excluded from rotation, explicit pinning, *and* the emergency path. There's a guard so the process-level fallback token (which belongs to one of the same Google accounts) can't leak traffic past the switch.
- Pause **everything** and the proxy answers `503 All accounts are paused` instead of serving behind your back.

`GRAVITY_ACCOUNT_CONCURRENCY` defaults to `1` — one account fully serialized, because Google rate-limits per credential. Raise it for parallel tool calls if you like living dangerously (throughput up, 429 risk up).

## Config

| Variable | Default | Purpose |
| --- | --- | --- |
| `GRAVITY_DATA_DIR` | `~/.anti-api` | creds, settings, caches |
| `GRAVITY_IDE_DB_PATH` | platform default | Antigravity `state.vscdb` path |
| `GRAVITY_HOST` | `127.0.0.1` | bind address |
| `GRAVITY_PORT` | `8964` | listen port (`-p` wins) |
| `GRAVITY_ACCOUNT_CONCURRENCY` | `1` | in flight per account (1–8) |
| `GRAVITY_ACCOUNT_INTERVAL_MS` | `1000` | min spacing per account |
| `GRAVITY_MIN_REQUEST_INTERVAL_MS` | `250` | global spacing |
| `GRAVITY_INSECURE_TLS` | unset | `1` disables TLS verify — read below |
| `GRAVITY_NO_OPEN` | unset | `1` skips dashboard auto-open |
| `GRAVITY_OAUTH_NO_OPEN` | unset | `1` skips browser for sign-in |
| `GRAVITY_SEARCH_TOKEN` | unset | require token on `/search` |
| `GRAVITY_IDE_VERSION` | `1.15.8` | pin the presented IDE version |
| `GRAVITY_USER_AGENT` | derived | full upstream UA override (debugging) |
| `GRAVITY_JITTER` | enabled | `0` disables transport jitter for deterministic runs |

`ANTI_API_*` equivalents are honoured, so old env keeps working.

> ⚠️ **Cheeky warning with teeth:** `GRAVITY_HOST` defaults to loopback, but an inherited `ANTI_API_HOST=0.0.0.0` binds *every interface* — an unauthenticated proxy holding Google credentials, reachable from your LAN. If you didn't mean to share your quota with the coffee shop, check your env. Changing it only affects new terminals; old ones keep the value until restarted.

**TLS:** the old TypeScript build disabled cert verification on *every* Google call. This port verifies by default. Behind a TLS-inspecting corporate proxy? `GRAVITY_INSECURE_TLS=1` restores the old behaviour — knowing it exposes your Google tokens to whatever terminates the connection.

## Honest caveats (measured, not guessed)

- **Sampling params don't work.** `temperature`/`top_p`/`top_k` are validated, forwarded under Gemini's field names, pinned by `TestSamplingParametersReachTheWire` — **and upstream drops them**. `temperature: 0` returned four different sentences in four calls; `temp 0 + top_k 1 + top_p 0` still varied on both endpoints, while `maxOutputTokens` in the same block *was* honoured. Don't rely on `temperature: 0` for determinism. No client change needed if upstream ever honours them.
- **`stop_reason` lies when truncated.** Reported `end_turn` even past `max_tokens`. Compare `usage.output_tokens` vs requested `max_tokens` instead.
- **Images must be base64.** URLs can't be inlined upstream. Non-base64 becomes a visible `[image omitted: …]` marker + a log line naming the exact shape (`GET /logs?json=1` to see it). The marker never echoes the original — an earlier version pasted megabytes of base64 back as prose, and the model confidently described a photo it never saw. A missing picture beats a hallucinated one.
- **Cost panel is cosplay.** Antigravity bills weekly quota, not tokens. Dollar figures are synthetic estimates; family fallbacks (`claude $5/$25`, `gemini $2/$12`, `gpt $1.75/$14` per M) are inherited guesses — flash's real promo rate is `$0.75/$3.75`. Check the rate table in the panel / `GET /usage`. When the promo ends, update `modelRates` in `internal/usage/usage.go` (a test pins it so the change is deliberate).

## What's NOT here (on purpose)

- Only the **antigravity provider**. No codex, copilot, zed, kiro, grok.
- No routing engine, no tunnel management — gone, not greyed out. Bring your own ngrok/cloudflared.
- No self-update. Rebuild: `go build -o gravity-go.exe .` (`GET /updates/check` says so, politely).
- No embeddings, Responses API, or image generation — explicitly `501`, not silently ignored.

## Performance notes (with receipts)

Benchmarks in `internal/antigravity/bench_*_test.go`:

- **SSE reading:** rescanned the whole buffer per 8KB read — O(n²) per event. 64KB event: 215 MB/s → **492 MB/s**. 1MB event: 23 MB/s → **474 MB/s**. Now scans only new bytes (+3 overlap for split separators).
- **Thought-signature store:** held the lock across JSON encode + ~1MB disk write while lookups queue on it. Snapshots under lock, writes outside: **3.4ms → 47.7µs** lock time.
- **Schema normalisation:** ~441µs for a 20-tool turn. Left alone — immaterial next to a multi-second upstream call.

## Checks

```bash
go run ./cmd/check        # format + vet + build + tests + race detector
go run ./cmd/check -short # skip slower cases
go test ./...             # just the tests
```

Race detector needs a C compiler (cgo). Missing one isn't a code defect — the checker says so and tells you what to install:

```bash
winget install -e --id BrechtSanders.WinLibs.POSIX.UCRT.Base   # Windows
```

Tests cover the wire-exact stuff: schema cleaning, signature store/recovery, SSE block-index state machine, retry classification, rotation + refresh coalescing under concurrency, SQLite reader, search, HTTP surface.

## Docs map

- [API.md](API.md) — the 5-minute integration reference.
- [LOCALAPI.md](LOCALAPI.md) — every route, every field, every status code. The contract.
- [DESIGN.md](DESIGN.md) — dashboard design tokens (borrowed binq.cc system). Only read if you're touching CSS.

---

*Built for the moment your paid plan says "slow down" and your deadline says "lol no." If gravity-go saved a deploy, star it so the next person drowning in 429s finds the lifeboat faster.*
