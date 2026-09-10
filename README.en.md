# gravity-go

**[العربية](README.md)**

> Let's be honest: the Antigravity app is rough, neglected, and nobody enjoys its interface.. but its quota and models are free, and free is too good to waste! So we pulled its API out from under the hood and wired it into OpenCode — finish your work in the editor you love without paying one extra cent.

Antigravity's internal models, running for you as a local API fully compatible with Anthropic and OpenAI. A lightweight Go service in **one static binary** — no Node, no CDN headaches, no sidecar files nagging you.

## The disaster that keeps happening

You paid good money for an OpenCode subscription, then mid-way through a long coding session the reply gets **rejected straight to your face** (happened in a real session that burned 669k tokens!), or you suddenly slam into the **100-tool ceiling**, or all work halts out of nowhere with a **429** while you've only got a few files left to finish — and the meter keeps charging while you zone out at the `retry-after` screen.

Instead of wrestling with Antigravity's miserable app and its tired experience, point OpenCode at `http://127.0.0.1:8964` and burn **Antigravity's weekly quota** off the shelf — Gemini + Claude working with you in the same agent loop. And when the first Google account runs dry, the request **quietly and automatically moves to the second account**. You get the full, correct reply without ever smelling an error.

## Why OpenCode users will love it

* **Both protocols supported:** `POST /v1/messages` (Anthropic) and `POST /v1/chat/completions` (OpenAI) — with streaming, tool calls, images, and thinking blocks.
* **Built for heavy sessions:** handles up to 100,000 messages and 10,000 tools per request.
* **Doesn't die when quota runs out:** one account serves you until it slaps you with `429 quota_exhausted` — then it rests the account, benches it to the reserves, and automatically retries with another account. All of it persisted even if you stop and restart the server.
* **Just one file:** dashboard embedded inside the binary (`go:embed`), lightweight and working offline with no internet.

Fastest wiring (from the [OpenCode providers docs](https://opencode.ai/docs/providers/), `baseURL` style):

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

Or as a dedicated local provider (same way as Ollama):

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

Use any dummy key you like (`ANTHROPIC_API_KEY=dummy`) — the server runs as a local loopback proxy, so it doesn't need a real key at all.

## How to run it?

All it asks for is **Go 1.25.7+**. Nothing else.

```bash
git clone https://github.com/<you>/gravity-go.git
cd gravity-go
go run .                    # exactly like running start
go build -o gravity-go.exe .
```

Runs on port **8964**, dashboard at `http://localhost:8964/quota`

```bash
gravity-go start [-p PORT] [-v]  # default port 8964
gravity-go login                 # add a Google account via OAuth
gravity-go accounts              # list your accounts
gravity-go logout-ide            # sign the editor out and reset the session
gravity-go version               # print the version number
```

## Login: two ways, in order

1. **Via OAuth** — run `gravity-go login` ← approve on Google's page ← tokens get saved to `~/.gravity-go/auth.json`. You can add more than one account and it switches between them automatically.
2. **Via the local IDE** — no OAuth session? It reads the token straight out of Antigravity's `state.vscdb` file (read-only, no modifications).

Data is stored in `~/.gravity-go`; if you have an old `~/.anti-api` folder it gets used as-is when the new folder doesn't exist. You can change the path manually with `GRAVITY_DATA_DIR`.

## Which models are available?

| Model | Details |
| --- | --- |
| `gemini-3.8-flash-high` | newest flash, the main and default pick |
| `gemini-3.7-flash-high` | the previous flash |
| `gemini-3.1-pro-high` | the strongest, for long deep-thinking sessions |
| `claude-opus-4-6-thinking` | its own independent weekly quota |

`GET /v1/models` works on a whitelist basis — you can call other models by name and it translates them internally (`-high` to the real `-tiered` ids).

* **Anthropic:** `POST /v1/messages` (plus `/v1beta/messages` and `/messages`).
* **OpenAI:** `POST /v1/chat/completions` — with `reasoning_effort` and `stream_options.include_usage`.
* **Search:** `GET|POST /search` — documented answers with their sources.
* **Dashboard:** `/quota`, `/quota/json`, `/usage`, `/settings`, `/logs`, `/auth/*`, `/accounts/*` — lightweight, no CDN hostage-taking, works offline.

Full docs: [API.md](API.md); the detailed contract for every route: [LOCALAPI.md](LOCALAPI.md).

## Failover and pausing

* Accounts serve you **one after another in saved order** — deliberately not quota-percentage based (cached quota numbers play games and don't reflect reality per request).
* You can pause an account from the dashboard or via `POST /accounts/{id}/enabled` — it keeps the login data and clears the cooldown as soon as you bring it back.
* Paused **all accounts**? It gives you a straight `503 All accounts are paused` instead of working behind your back and spending without you knowing.

## Settings and environment variables

| Variable | Default | What does it do? |
| --- | --- | --- |
| `GRAVITY_DATA_DIR` | `~/.gravity-go` | data and settings path |
| `GRAVITY_HOST` / `GRAVITY_PORT` | `127.0.0.1` / `8964` | host and port binding (passing `-p` overrides this) |
| `GRAVITY_ACCOUNT_CONCURRENCY` | `1` | how many requests run at once per account (1 to 8) |
| `GRAVITY_ACCOUNT_INTERVAL_MS` | `1000` | minimum gap between one account's requests (milliseconds) |
| `GRAVITY_MIN_REQUEST_INTERVAL_MS` | `250` | global request spacing |
| `GRAVITY_SEARCH_TOKEN` | unset | requires a protection token for `/search` |
| `GRAVITY_NO_OPEN` / `GRAVITY_OAUTH_NO_OPEN` | unset | set `1` to stop it auto-opening the browser |
| `GRAVITY_INSECURE_TLS` | unset | set `1` to disable TLS checking (corporate proxies only) |
| `GRAVITY_JITTER` | enabled | set `0` to kill randomness and keep the path fixed |

> ⚠️ Heads up: setting `GRAVITY_HOST=0.0.0.0` makes it listen on *every interface* — an open proxy with no password carrying your Google credentials across the whole local network. Check your variables unless you fancy treating the café customers to your quota without knowing.

## To be straight with you (real experiments)

* **Sampling params don't work:** options like `temperature`/`top_p`/`top_k` reach the upstream server and get completely ignored. We tried `temperature: 0` and got back four different sentences!
* **`stop_reason` sometimes bluffs:** if the text feels cut off, manually compare `usage.output_tokens` against `max_tokens`.
* **Images must be Base64:** send an image URL and it turns into a clear text note `[image omitted: …]` instead of the model hallucinating a description out of thin air.
* **The dollar cost math is for show:** the accounting is a weekly quota, not per-token billing — all those dollar figures are just for vibes so you can picture the volume.

## Things we excluded on purpose

The program exists to pull Antigravity services and exploit them, nothing else. Don't expect advanced network routing, tunnels and hole-punching (bring your own ngrok), auto-update, or embeddings — any unsupported route slaps you with a straight `501`, no beating around the bush.

## Checking the code

```bash
go run ./cmd/check    # formatting + vet + build + tests + race detection
go test ./...         # tests only
```

The race detector needs a C compiler: install it with `winget install -e --id BrechtSanders.WinLibs.POSIX.UCRT.Base`

## Docs

* [README.md](README.md) — the Arabic version.
* [API.md](API.md) — quick 5-minute integration reference.
* [LOCALAPI.md](LOCALAPI.md) — full docs for every route.
* [DESIGN.md](DESIGN.md) — dashboard design system (CSS only).

---

*Made for the moment your paid plan tells you to "calm down and slow-play it" while the deadline answers with an evil laugh saying "yeah, right". If gravity-go bailed you out and saved a delivery on the edge, don't skimp on a Star so the next person drowning in 429 rate limits finds the lifeboat faster.*
