# `context-guru`

Compacts an agent's outbound LLM request context before forwarding,
using the embedded context-guru engine. `OnResponse` is currently a
pass-through; model-driven expand/restore is a later integration.
Opt-in at build time (`-tags include_plugin_contextguru`) because its
engine pulls a large transitive dependency set.

- `paths` (`[]string`) — inference request paths to compact. Default: `/v1/chat/completions`, `/v1/completions`, `/v1/messages`.
- `model` (object) — optional "cheap" LLM endpoint for model-backed components (summarize, extract:code); omitted means those degrade to deterministic/no-op.
  - `base_url` — OpenAI-compatible endpoint base.
  - `model` — model name to call.
  - `api_key` — optional bearer token.
  - `max_tokens` — completion cap, default 4096.
  - `timeout_ms` — per-call timeout, default 150000.
- `engine` (object) — native context-guru config (preset / pipeline / per-component / store), passed through verbatim. Default: `preset: balanced`.
