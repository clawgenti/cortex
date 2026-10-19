# `opa`

Evaluates [OPA](https://www.openpolicyagent.org/docs) (Open Policy Agent)
policy bundles against inbound and outbound requests, using an embedded
OPA engine and four fixed decision paths. Full details in the plugin's
[README](../core/plugins/opa/README.md).

- `bundle_url` (string) — base URL of the Rossoctl Bundle Server, the in-cluster service that serves per-agent [OPA policy bundles](https://www.openpolicyagent.org/docs/management-bundles) keyed by SPIFFE ID (see [how it works](../core/plugins/opa/README.md#how-it-works)); required.
- `agent_id_file` (string) — path to the agent's client-ID file. Default `/shared/client-id.txt`.
- `agent_id` (string) — inline agent ID; overrides `agent_id_file` when set.
- `polling_min_delay` / `polling_max_delay` (int) — bundle polling interval bounds in seconds. Defaults 10 / 120.
- `include` (`[]string`) — optional field groups exposed in the OPA input document (e.g. `mcp.params`, `a2a.content`, `inference.messages`); default lean/empty.
