# discovery-exchange plugin

`discovery-exchange` is an outbound token-exchange plugin that needs **no
routes**. It works out a target audience from the destination host, and lets
Keycloak decide whether that destination is a target:

- Keycloak issues a token → the request goes out carrying it.
- Keycloak has no client by that name → the request passes through unchanged.
- Any other answer → the request is denied.

Adding a target is then a Keycloak change only. No `authproxy-routes`
ConfigMap, no `routes.yaml`, no restart:

```yaml
# Not needed with discovery-exchange
data:
  routes.yaml: |
    - host: "github-tool-mcp"
      target_audience: "github-tool"
      token_scopes: "openid github-tool-aud github-full-access"
```

It is an **alternative** to [`token-exchange`](./plugin-catalog.md#token-exchange),
not a complement. Both rewrite the outbound `Authorization` header, so put
one or the other on a chain, never both. The two share no code, so a change to
one can never quietly change the other.

## What happens to each request

| Destination | Keycloak's answer | Result | Session event |
|---|---|---|---|
| Host outside the cluster (`api.openai.com`, an IP, `github-tool.evil.example`) | not asked | forwarded unchanged | `skip / host_not_in_cluster` |
| In-cluster, audience granted to this workload | 200, token for that audience | `Authorization` replaced | `modify / token_exchanged` (or `cache_hit`) |
| In-cluster, no Keycloak client by that name | 400 `invalid_client` "Audience not found" | forwarded unchanged; remembered for `unknown_audience_ttl` | `skip / audience_unknown` |
| In-cluster, client exists but is **not granted** to this workload | 400 `invalid_request` "Requested audience not available" | **403** | `deny / audience_not_granted` |
| Keycloak down, timeout, 5xx, bad credentials, any other error | — | **503** | `deny / exchange_failed` |
| Credentials not yet loaded from `/shared/` | not asked | **503** | `deny / credentials_not_ready` |

Two rules hold throughout:

- **Passthrough is caused by exactly one answer:** "Audience not found". Every
  other failure, including an outage, denies. If a Keycloak upgrade rewords that
  message, unknown destinations start being denied; nothing is ever wrongly
  passed through.
- **The token sent to X is always a token for X.** The plugin checks the `aud`
  claim of every token Keycloak issues and denies if the requested audience is
  missing. It never sends an exchange without an audience: Keycloak answers that
  with the caller's broadest token.

Agents and tools are handled the same way. To Keycloak, an agent you call is
just another client, and the AuthBridge sidecar in front of it validates the
token's audience exactly as a tool's does.

## Which hosts are offered to Keycloak

The audience comes from the Host header, and the workload chooses the host,
possibly because a prompt told it to. If every host were eligible, a request to
`github-tool.evil.example` would ask for the `github-tool` audience, get a real
token, and send it to `evil.example`. So only names that Kubernetes DNS resolves
inside the cluster are eligible:

| Host form | Example | Namespace taken from |
|---|---|---|
| short name | `github-tool`, `github-tool:9090` | the `namespace` setting |
| `service.namespace.svc` | `github-tool.team1.svc` | the host |
| FQDN | `github-tool.team1.svc.cluster.local` | the host |

`github-tool.team1` is **not** eligible. Kubernetes resolves it, but it has the
same shape as `example.com`. IP literals, extra labels, a different cluster
domain, and anything that is not an RFC 1123 label are refused too. A refused
host is forwarded unchanged and never reaches Keycloak.

Anyone who can create a Service can still pick a name that matches a Keycloak
client. That is the trust every in-cluster caller already places in cluster DNS.

## Configuration

```yaml
# authbridge-runtime-config: pipeline.outbound.plugins
pipeline:
  outbound:
    plugins:
      - name: discovery-exchange
        config:
          keycloak_url: "${KEYCLOAK_URL}"
          keycloak_realm: "${KEYCLOAK_REALM}"
          # Everything below is optional; these are the defaults.
          client_id_file: /shared/client-id.txt
          client_secret_file: /shared/client-secret.txt
          audience_template: "{service}"
          cluster_domain: cluster.local
          no_token_policy: client-credentials
          unknown_audience_ttl: 5m
          timeout: 5s
```

| Field | Default | Meaning |
|---|---|---|
| `token_url` | derived | Token endpoint. Required unless `keycloak_url` and `keycloak_realm` are set. |
| `keycloak_url`, `keycloak_realm` | — | Keycloak's **internal** service URL and realm; `token_url` becomes `<url>/realms/<realm>/protocol/openid-connect/token`. |
| `client_id` / `client_id_file` | `/shared/client-id.txt` | This workload's client ID. The inline value wins. |
| `client_secret` / `client_secret_file` | `/shared/client-secret.txt` | This workload's client secret. The inline value wins. |
| `audience_template` | `{service}` | Audience requested for a destination. Placeholders: `{service}`, `{namespace}`. Must contain `{service}`. |
| `namespace` | — | Namespace assumed for short names, for templates that use `{namespace}`. Usually `${POD_NAMESPACE}`. Without it, short names pass through with `skip / namespace_unknown`. |
| `cluster_domain` | `cluster.local` | Cluster DNS domain, for recognising FQDNs. |
| `no_token_policy` | `client-credentials` | The request carries no bearer token: `client-credentials` exchanges the workload's own token (see below), `passthrough` forwards it, `deny` answers 401. |
| `unknown_audience_ttl` | `5m` | How long a destination Keycloak doesn't know is passed through without asking again. `0` asks on every request. |
| `timeout` | `5s` | Timeout for each call to the token endpoint. |

There is no `routes`, `token_scopes`, `default_policy` or `audience_from_host`
field. Configure rejects unknown fields, so a `token-exchange` config pasted in
fails at load instead of half-working.

The credential files work as they do for `token-exchange`. The plugin reads them
when it is configured, and if they aren't there yet it waits for them in the
background, holding the pod not-ready until they land. Plugin config
hot-reloads, so editing it needs no restart.

### Choosing `audience_template`

The audience must be the target's Keycloak **client ID**, which is also what the
target's own sidecar validates as `aud`.

- **Clients named after their Service** (`github-tool` serving at
  `github-tool`): use the default, `{service}`.
- **Operator-registered workloads**, whose client ID is a SPIFFE ID:
  `spiffe://<trust-domain>/ns/{namespace}/sa/{service}`, with
  `namespace: ${POD_NAMESPACE}` for short names.

The SPIFFE form only works where the **service account has the same name as the
Service**. The Host header carries the Service name, never the service account.
The weather-agent advanced demo breaks this: Service `weather-tool-advanced-mcp`,
service account `weather-tool-advanced`. For such a target the derived audience
names no client, so the request passes through and Keycloak never issues a token
for it. Rename one of the two, or keep `token-exchange` for that target.

## Keycloak requirements

All four are needed. Each one, when missing, breaks the plugin in a different
way.

1. **Standard token exchange enabled on the caller's client.** Set
   `standard.token.exchange.enabled=true` (Keycloak 26.2+). Without it the
   legacy provider answers, and every granted target gets 403 "Client not
   allowed to exchange". Every request to a granted target is then denied.
2. **One client per target**, named exactly as `audience_template` renders it.
   Agents count as targets.
3. **Grant a target with a DEFAULT client scope on the caller's client**: a
   client scope holding an audience mapper for the target, attached to the
   caller as a *default* scope. The plugin sends no `scope` parameter, so
   *optional* scopes are never applied, and a target granted only through one
   looks not granted (403).
4. **Not a realm default scope.** A realm default reaches every client in the
   realm, so every caller is granted every target. "Not granted" then never
   happens, and each target's grant list means nothing. The github-issue
   demo's `agent-team1-git-issue-agent-aud` is set up this way; don't copy it.

Turning off the legacy `token-exchange` preview feature on the server is
recommended. It removes any doubt about which provider is answering.

### What "default scopes" costs

A token issued to the caller **without** an audience carries every target it is
granted, so it is accepted by all of them:
- a `client_credentials` token;
- an exchange with no audience;
- a user logging in directly through the agent's client.

The plugin never forwards such a token. An exchange is always narrowed to one
audience, and with `no_token_policy: client-credentials` the workload's own
token is used only as the subject of a second, narrowing exchange. Anything
*else* that obtains a token for that client gets the broad one.

The `scope` claim of a narrowed token still lists **all** the caller's scope
names, even though `aud` holds one. So each target can see which other targets
the caller can reach, and scope-based authorization at a target (e.g. a
`github-full-access` scope) no longer differs from caller to caller.

## No bearer token on the request

With `no_token_policy: client-credentials` (the default), the plugin gets the
workload's own token with `client_credentials` (cached until shortly before it
expires) and exchanges it for the destination's audience. That second step is
required, not an optimisation: Keycloak ignores `audience` on
`client_credentials` and returns a token for every granted target.

Discovery works the same way without a user token. An unknown destination still
passes through, and the workload's own token is never attached to it.

## Caching and timing

- **Issued tokens** are cached per (subject token, audience) until 30s before
  they expire. The cache key is a SHA-256 of the subject, so the cache holds no
  usable copy of a caller's token.
- **Unknown destinations** are remembered for `unknown_audience_ttl`. Granting a
  client that already exists takes effect on the next request. A client
  *created* for a destination already remembered as unknown takes effect within
  the TTL.
- Each cache holds at most 10,000 entries. Revoking a grant does not invalidate a
  token already issued; it lives until it expires.

## Observability

Every request records one invocation in the session stream. The reasons are in
the table at the top; `details` carries `host`, `target_audience`, and
`cache_hit` or `cached`. `GET /v1/pipeline` shows these counters, which reset on
restart and on config reload:

| Metric | Counts |
|---|---|
| `discovery-exchange.exchanged` | tokens Keycloak issued |
| `discovery-exchange.cache_hits` | requests served from the token cache |
| `discovery-exchange.unknown_passthrough` | requests to destinations Keycloak has no client for |
| `discovery-exchange.not_eligible` | requests to hosts never offered to Keycloak |
| `discovery-exchange.denied` | requests denied, for any reason |

The WARN log for a failed exchange includes Keycloak's error. The 503 body sent
to the workload deliberately doesn't, because that error can name other
audiences the caller holds.

## Limitations

- **Keycloak only, in practice.** The two answers that mean "unknown" and "not
  granted" are Keycloak's error texts, confirmed against Keycloak 26.5. Against
  another IdP, every failure is a deny, so no destination is ever
  discovered as unknown.
- **Client-secret identity only.** No SPIFFE JWT-SVID client assertion yet.
- **No scopes.** Grants have to be default client scopes, as described above.
- **Plaintext HTTP only**, like every outbound plugin. An opaque HTTPS tunnel
  reaches no plugin, so it is neither exchanged nor checked.
- **The service account and the Service must share a name** for SPIFFE-ID
  audiences. See [Choosing `audience_template`](#choosing-audience_template).

## Moving the github-issue demo to it

1. In the agent's runtime config, replace the `token-exchange` entry with the
   `discovery-exchange` example above.
2. Delete the `authproxy-routes` ConfigMap.
3. In Keycloak, enable standard token exchange on the agent's client.
   - Attach `github-tool-aud` as a **default** scope of the agent's client.
   - Remove it from the realm defaults if `setup_keycloak.py` put it there.
4. Check the target's client ID is the name the agent calls: the agent calls
   `github-tool-mcp`, and the client is `github-tool`. Either name the client
   `github-tool-mcp`, or rename the Service. (Under `token-exchange`, a route
   mapped one name to the other; nothing does that here.)
5. `github-full-access` cannot be requested any more. If the tool should grant it,
   make it a default scope of the agent's client too, accepting that every token
   the agent gets for any target now lists it.

## Verifying against a real Keycloak

The unit tests use a fake Keycloak that copies the probed answers. To check the
real error texts before upgrading Keycloak, run the live test. It creates a
throwaway realm through the admin API and deletes it afterwards:

```sh
cd core
DISCOVERY_EXCHANGE_LIVE_KEYCLOAK=http://keycloak.localtest.me:8080 \
  go test -run TestLiveKeycloak -v ./plugins/discoveryexchange/
```

It logs in as `admin`/`admin` unless `DISCOVERY_EXCHANGE_LIVE_ADMIN` and
`DISCOVERY_EXCHANGE_LIVE_PASSWORD` say otherwise. It is skipped when the variable
is unset, so CI never runs it.
