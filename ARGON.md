# Gemini 4 Argon

> **Status as of 2026-10-24: the proxy accepts Argon, but it has never been
> confirmed against the real service.** The maintainer had no account with Argon
> access, so nobody has seen a successful Argon response through this project yet.
> This file separates what was observed from what is assumed, and says how to finish
> the job if you have an account that can use it.

## For AI assistants

If a user asks you to use, fix or expose Argon in this repository, read this whole
file first, then follow these rules.

1. Treat the upstream id `gemini-4-argon` as a hypothesis until step 3 of
   [Finishing the job](#finishing-the-job) has returned a 200 on a real account.
2. Do not add Argon to `visibleModels` (what `/v1/models` and the dashboard list)
   unless the user asks. Most people running this proxy are on free accounts, where
   Argon answers 404.
3. This repository is public. Never write account emails, tokens, project ids or raw
   API responses that contain them into code, tests, docs, commits or issues.
4. Every call spends the account's Antigravity usage allowance. Keep probes tiny
   (`max_tokens` of 16 is enough) and stop as soon as you have the answer.
5. A 404 on an Argon id has two causes that look identical: the account has no
   access, or the id is wrong. Do not conclude either from a 404 alone.

## What Argon is

Everything in this section comes from public sources (links at the end), not from
this project.

- Google's Gemini 4 flagship model, reported as announced on 2026-09-30 and broadly
  available since 2026-10-13. Public Gemini API id: `gemini-4-argon`. Thinking
  levels `low`, `medium` and `high`; a 1,048,576-token context; 65,536 tokens of
  default output.
- Gemini API price: $2.00 per million input tokens and $10.00 per million output
  tokens (thinking counts as output) through 2026-11-30, then $4.00 and $20.00. Not
  on the API free tier. Antigravity bills by quota, not by token, so this only
  matters for the cost estimates on the dashboard's usage page.
- In Antigravity it rolled out from 2026-10-13 to paid plans, with a **separate,
  weekly-only Argon quota** on top of the existing Gemini quota. Reports disagree on
  whether Google AI Pro qualifies or only Ultra. It is reported to appear in the
  Antigravity desktop app and CLI (`/model`) but not in the standalone IDE, and the
  rollout was gradual.

## Using it through this proxy

You need an Antigravity account whose plan includes Argon. Add it with "add account"
on the dashboard, or with `gravity-go login`.

| id you send | upstream model | thinking level |
| --- | --- | --- |
| `gemini-4-argon` | `gemini-4-argon` | not set, left to upstream |
| `gemini-4-argon-low` | `gemini-4-argon` | `low` |
| `gemini-4-argon-medium` | `gemini-4-argon` | `medium` |
| `gemini-4-argon-high` | `gemini-4-argon` | `high` |

The suffixed names are what Antigravity's own client calls these variants. As with
`gemini-3.8-flash-*`, the suffix is client-facing only: it becomes
`thinkingConfig.thinkingLevel`, and the model id sent upstream is the same for all
of them. **That upstream id is the unconfirmed part.**

The ids are callable but deliberately **not listed** in `/v1/models` or on the
dashboard, the same treatment as the gated Cyber models.

```bash
curl http://127.0.0.1:8964/v1/messages \
  -H 'content-type: application/json' \
  -d '{"model":"gemini-4-argon-high","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}'
```

For OpenCode, add it under the provider's `models`:

```json
"gemini-4-argon-high": { "name": "Gemini 4 Argon (High)" }
```

**Quota.** Argon's weekly-only quota should show up as an extra group in
`/quota/json` for an entitled account. That has not been observed. Argon is
deliberately not part of the single "Gemini" bar the dashboard builds from per-model
quota (`geminiProModelIDs` and `geminiFlashModelIDs` in
`internal/antigravity/quota.go`): a separate quota folded into a shared one would
make that bar misreport.

## What was checked on 2026-10-24

| Question | Finding |
| --- | --- |
| Does Antigravity's own client know the model? | Yes. The Antigravity IDE 2.25.0 language server (`resources/bin/language_server.exe`) and the `agy` CLI both contain the strings `gemini-4-argon`, `gemini-4-argon-low`, `gemini-4-argon-medium`, `gemini-4-argon-high` and the label "Gemini 4 Argon (High)"; the CLI also has a `defaultmodels.Gemini4Argon` symbol. Found with a plain byte search of the binaries. The same tables list `claude-opus-5-5-{low,max}` and `claude-sonnet-5-5-{low,max}`, which this proxy does not handle. |
| Do accounts without Argon see it? | No. On two such accounts `fetchAvailableModels` returned 19 models, none containing "argon", and the word is nowhere in the response. The quota summary had one group, "Claude and GPT models". |
| Where would it appear? | In that response `tieredModelIds` was `flash: gemini-3.8-flash-tiered`, `pro: gemini-3.8-flash-tiered`, `flashLite: gemini-3.5-flash-lite`: the `pro` slot falls back to the 3.8 Flash model. **Unverified guess:** on an entitled account that slot, and the catalogue, name Argon's real upstream id. |
| Can upstream tell "wrong id" from "no access"? | No. On `streamGenerateContent` (daily endpoint, tiny request) `gemini-4-argon`, `gemini-4-argon-tiered`, `-low`, `-medium`, `-high`, `-agent`, `gemini-argon-agent`, a made-up id and the gated `gemini-3.8-flash-cyber-tiered` all returned the identical `404 NOT_FOUND "Requested entity was not found."`. The control `gemini-3.8-flash-tiered` returned 200. The other two entries of `baseURLs` were not tried. |

### Dead ends, so you don't repeat them

- `agy models` while the CLI is not logged in prints a built-in default list. Its
  log says "You are not logged into Antigravity" and that it skipped
  `fetchAvailableModels`. That list says nothing about entitlement.
- The proxy's IDE-token fallback needs a signed-in IDE. The test machine had none.
- Google's open-source `gemini-cli` model list (`packages/core/src/config/models.ts`)
  has no Argon entry, and web searches found no community Antigravity proxy that
  lists Argon's id.
- A model missing from `fetchAvailableModels` is not proof it is unavailable: 3.8
  Flash served normally before it was listed.

## Finishing the job

This needs an account that can really use Argon.

1. **Add the account** (dashboard "add account"). For the test, switch your other
   accounts off on the dashboard: the proxy and the probe below both use the first
   enabled account.
2. **Read its catalogue.**

   ```bash
   PROBE=1 go test ./internal/antigravity/ -run TestProbeUpstreamModelCatalogue -v
   ```

   In PowerShell: `$env:PROBE='1'; go test ./internal/antigravity/ -run TestProbeUpstreamModelCatalogue -v`.
   Set `GRAVITY_DATA_DIR` if your data directory is not the default. Look for a key
   containing `argon`. If there is one, that key is the upstream id: skip to step 4.
3. **If the catalogue does not list it, try candidates.** It can lag the rollout.
   Ids the proxy does not map pass through unchanged, so send the candidate as
   `model` through the running proxy, with a tiny request:

   ```bash
   curl http://127.0.0.1:8964/v1/messages \
     -H 'content-type: application/json' \
     -d '{"model":"gemini-4-argon-tiered","max_tokens":16,"messages":[{"role":"user","content":"Say OK"}]}'
   ```

   Try `gemini-4-argon` (the current mapping), `gemini-4-argon-tiered`, then every
   id in the raw catalogue's `tieredModelIds`. A 200 with text proves both the
   entitlement and the id. A 404 on all of them while a control such as
   `gemini-3.8-flash-high` works means the account has no Argon access, or the id is
   something else: check the model picker in the Antigravity app or CLI first. The
   `-low`, `-medium` and `-high` names are client ids and are mapped, so to test
   per-level upstream ids, change `modelMapping` temporarily.
4. **Record the answer** in `internal/antigravity/models.go`. Point the four
   `gemini-4-argon*` entries of `modelMapping` at the confirmed id (keep the
   suffix-to-thinking-level behaviour unless upstream really needs per-level ids),
   delete the "NOT CONFIRMED ON THE WIRE" comment, update the `TestArgon*` tests in
   `models_test.go` and `translate_test.go` (they currently assert
   `gemini-4-argon`), and run `go test ./...`.
5. **Update the docs:** the status banner above and the Argon section of `API.md`.
   Note which plan (Pro or Ultra) worked.
6. **Quota and cost:** check `/quota/json` for an Argon row and give its bucket id a
   friendly label in `bucketLabels` (`internal/antigravity/quota.go`). Optionally add
   a price to `modelRates` in `internal/usage/usage.go`; without one, Argon is
   estimated at the generic Gemini rate.
7. **Visibility is a separate decision.** Add the ids to `visibleModels` only if
   listing a plan-gated model to everyone is acceptable.
8. **Share the result** (issue or PR): the catalogue key, the id that returned 200
   and the plan. No tokens, emails or raw responses that contain them.

## Where things live

- `internal/antigravity/models.go`: `AvailableModels` (the callable catalogue),
  `modelMapping` (client id to upstream id), `visibleModels` (what listings show).
- `internal/antigravity/translate.go`: `thinkingSuffix` and `buildUpstreamRequest`
  turn a `-low`, `-medium` or `-high` suffix into `thinkingConfig.thinkingLevel` for
  any upstream id containing "gemini".
- `internal/antigravity/quota.go`: the quota groups, with Argon intentionally left
  out.
- Tests: the `TestArgon*` functions in `internal/antigravity/models_test.go` and
  `internal/antigravity/translate_test.go`.
- `API.md`: the short user-facing note on Argon.

## Sources

Read on 2026-10-24.

- [Gemini 4 Argon, Gemini API docs](https://ai.google.dev/gemini-api/docs/models/gemini-4-argon)
- [Gemini API pricing](https://ai.google.dev/gemini-api/docs/pricing)
- [9to5Google, on the Antigravity rollout and its separate quota](https://9to5google.com/?p=733502)
- [Gemini 4 Argon arrives in Antigravity for AI Pro with a separate quota](https://pasqualepillitteri.it/en/news/24640/gemini-4-argon-antigravity-ai-pro-separate-weekly-quota)
- [HPCwire, on the launch](https://www.hpcwire.com/bigdatawire/2026/10/14/gemini-4-argon-arrives-with-big-claims-for-ai-agents-and-scientific-discovery/)
