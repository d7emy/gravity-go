# gravity-go

**[العربية](README.md)**

> Your OpenCode subscription just hit a rate limit mid-refactor. Antigravity didn't. This is the bridge.

Antigravity's built-in models as a local Anthropic-compatible (and OpenAI-compatible) API. A Go service shipping as **one static binary** — no Node, no CDN, no sidecar files.

## The crisis

You already pay for OpenCode. Then a long session gets **rejected outright** (happened to a real 669k-token session), a tool-heavy setup slams into a **100-tool ceiling**, a refactor dies on a **429** with three files to go, and the meter keeps running while you stare at `retry-after`.

Point OpenCode at `http://127.0.0.1:8964` and burn **Antigravity's weekly quota** instead — Gemini + Claude, same agent loop. When one Google account taps out, the request **transparently fails over to the next account**. You see one good response, not an error.

## Why OpenCode users care

- **Both protocols:** `POST /v1/messages` (Anthropic) and `POST /v1/chat/completions` (OpenAI) — streaming, tools, images, thinking blocks.
- **Built for long sessions:** 100,000 messages and 10,000 tools per request.
- **Survives quota death:** one account serves everything until `429 quota_exhausted` — then cooldown, demote, transparent retry, persisted across restarts.
- **One file:** embedded dashboard (`go:embed`), works offline.

Quickest wiring ([OpenCode provider docs](https://opencode.ai/docs/providers/) `baseURL` pattern):

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

Or as a dedicated local provider (Ollama-style shape):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "gravity-go": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "gravity-go (local)",
      "options": { "baseURL": "http://127.0.0.1:8964/v1" },
      "models": {
        "gemini-3.8-flash-high": { "name": "Gemini 3.8 Flash (High)" }
      }
    }
  }
}
```

Dummy key everywhere (`ANTHROPIC_API_KEY=dummy`) — loopback proxy, no key needed.

## Run

Requires **Go 1.25.7+** only.

```bash
git clone https://github.com/<you>/gravity-go.git
cd gravity-go
go run .                    # same as start
go build -o gravity-go.exe .
```

Serves on **8964**, dashboard at http://localhost:8964/quota

```bash
gravity-go start [-p PORT] [-v]  # default 8964
gravity-go login                 # add a Google account via OAuth
gravity-go accounts              # list accounts
gravity-go logout-ide            # sign the IDE out, clear its session
gravity-go version               # print version
```

## Auth: two paths, in order

1. **OAuth** — `gravity-go login` → Google consent → tokens in `~/.gravity-go/auth.json`. Multiple accounts rotate automatically.
2. **Local IDE** — no OAuth session? Token is read from Antigravity's `state.vscdb` (read-only).

Data lives in `~/.gravity-go`; a legacy `~/.anti-api` dir is used as-is when the new one doesn't exist yet. Override with `GRAVITY_DATA_DIR`.

## What you get

| Model | Notes |
| --- | --- |
| `gemini-3.8-flash-high` | newest flash, default pick |
| `gemini-3.7-flash-high` | previous flash |
| `gemini-3.1-pro-high` | stronger, long thinking |
| `claude-opus-4-6-thinking` | separate weekly quota |

`GET /v1/models` is a whitelist — more ids are callable by name, mapped internally (`-high` → `-tiered` wire ids).

- **Anthropic:** `POST /v1/messages` (+`/v1beta/messages`, `/messages`).
- **OpenAI:** `POST /v1/chat/completions` — `reasoning_effort`, `stream_options.include_usage`.
- **Search:** `GET|POST /search` — grounded answer + sources.
- **Dashboard:** `/quota`, `/quota/json`, `/usage`, `/settings`, `/logs`, `/auth/*`, `/accounts/*` — no CDN, offline-friendly.

Full reference: [API.md](API.md); every-route contract: [LOCALAPI.md](LOCALAPI.md).

## Rotation & pausing

- Accounts serve **one at a time, in stored order** — deliberately not quota-aware (cached percentages lie per-request).
- Pause via dashboard or `POST /accounts/{id}/enabled` — keeps creds, clears old cooldown on resume.
- Pause **everything** → `503 All accounts are paused` instead of serving behind your back.

## Config

| Variable | Default | Purpose |
| --- | --- | --- |
| `GRAVITY_DATA_DIR` | `~/.gravity-go` | data & settings |
| `GRAVITY_HOST` / `GRAVITY_PORT` | `127.0.0.1` / `8964` | bind (`-p` wins) |
| `GRAVITY_ACCOUNT_CONCURRENCY` | `1` | in flight per account (1–8) |
| `GRAVITY_ACCOUNT_INTERVAL_MS` | `1000` | min spacing per account |
| `GRAVITY_MIN_REQUEST_INTERVAL_MS` | `250` | global spacing |
| `GRAVITY_SEARCH_TOKEN` | unset | require token on `/search` |
| `GRAVITY_NO_OPEN` / `GRAVITY_OAUTH_NO_OPEN` | unset | `1` skips browser launch |
| `GRAVITY_INSECURE_TLS` | unset | `1` disables TLS verify (corporate proxies only) |
| `GRAVITY_JITTER` | enabled | `0` for deterministic runs |

> ⚠️ `GRAVITY_HOST=0.0.0.0` binds *every interface* — an unauthenticated proxy holding Google credentials on your LAN. Check your env if you didn't mean to share quota with the café.

## Honest caveats (measured)

- **Sampling params don't work:** `temperature`/`top_p`/`top_k` reach the wire (pinned by test) and upstream drops them. `temperature: 0` returned four different sentences.
- **`stop_reason` lies on truncation:** compare `usage.output_tokens` vs `max_tokens`.
- **Images must be base64:** URLs become a visible `[image omitted: …]` marker rather than a hallucinated description.
- **Usage dollars are cosplay:** weekly quota, not per-token billing — all figures synthetic.

## NOT included (on purpose)

Antigravity provider only. No routing, no tunnels (bring ngrok), no self-update, no embeddings — rejected surfaces answer explicit `501`.

## Checks

```bash
go run ./cmd/check   # format + vet + build + tests + race
go test ./...        # tests only
```

Race detector needs a C compiler: `winget install -e --id BrechtSanders.WinLibs.POSIX.UCRT.Base`

## Docs

- [README.md](README.md) — Arabic version.
- [API.md](API.md) — 5-minute integration reference.
- [LOCALAPI.md](LOCALAPI.md) — the full contract.
- [DESIGN.md](DESIGN.md) — dashboard design tokens, CSS only.

---

*Built for the moment your paid plan says "slow down" and your deadline says "lol no." If gravity-go saved a deploy, star it so the next person drowning in 429s finds the lifeboat faster.*
