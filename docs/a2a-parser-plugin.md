# `a2a-parser`

Parses A2A JSON-RPC 2.0 request bodies into `pctx.Extensions.A2A`
(method, session ID, message parts, role) for downstream guardrails.

No configuration — registered as a bare plugin name, no `config:` block.
