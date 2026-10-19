# Supervisor fallback listener: a legible refusal when the proxy is not serving

**Date:** 2026-10-13
**Status:** designed
**Issue:** #1002

## Goal

When Cortex is routed into a coding agent's environment but is not serving, the agent
gets an error that names Cortex and the command that fixes it, instead of a bare
connection-refused that points at nothing.

## The problem

`agentop service install` writes `HTTPS_PROXY` into the agent's settings. The routing
outlives the proxy: when nothing is listening on the forward address, every request the
agent makes fails at the connect, and the agent reports a generic network error. Nothing
in it names Cortex, so the user has no thread to pull.

Three mitigations already cover part of this, and they narrow the work considerably:

| Case | Covered today by |
|---|---|
| `agentop service stop` | `serviceControl` (`cmd/agentop/cmd_service.go:795`) counts established connections first, then prints that clients cannot fall back to a direct connection and will keep failing until `agentop service start` |
| `agentop uninstall` | `unrouteClaudeCode` / `unrouteByHand` (`cmd/agentop/cmd_uninstall.go`) genuinely remove the routing for Claude Code, OpenCode and Codex |
| Diagnosis after the fact | `checkServing` (`cmd/agentop/cmd_doctor_checks.go:113`) probes `/healthz` then `/readyz` and reports `not answering <url>` with the restart command |

What remains uncovered is every case where **nobody ran `stop`**:

- the child crash-looping on a config it cannot load,
- the child killed out from under the supervisor,
- a half-finished install that routed before the proxy came up,
- the long `superviseBlockedMaxDelay` waits a port conflict puts the supervisor into
  (#1122 / #1366).

In all of these no warning was ever printed, because no command was ever run. The user
meets the failure at the moment of use, with nothing to go on.

## What the probe established

The design rests on a claim worth verifying before building on it: that a fail-closed
`502` is more legible than a refused connection. It is, but no single field of the
response reaches every client. Measured against a stub proxy that refuses `CONNECT` with
a custom reason phrase and a diagnostic body:

| Client | What the user sees | Channel |
|---|---|---|
| `curl` 8.7.1 | `curl: (56) CONNECT tunnel failed, response 502` | neither — discards both |
| Go `net/http` (`gh`, most CLIs) | `Get "https://…": Cortex proxy is not serving` | reason phrase |
| Claude Code | `API Error: 502 Cortex is routed in this environment (HTTPS_PROXY) but is not serving. Run: agentop service start` | body |

Two consequences for the design:

- **Both fields must be set.** Claude Code reads the body, Go reads the reason phrase.
- **The response must be written on the raw socket.** Go's `net/http` always writes the
  canonical status text and offers no way to set a custom reason phrase, so the existing
  `http.Error(w, …, 502)` shape at `core/listener/forwardproxy/server.go:1907` cannot
  carry the Go-visible half of this. The fallback writes bytes directly.

Two incidental findings, recorded because they shape expectations rather than code:

- **`~/.claude/settings.json`'s `env` block overrides the shell environment.** Setting
  `HTTPS_PROXY` in a subprocess env did not re-route Claude Code; it used the settings
  value. The file agentop writes is the authoritative routing, and a shell variable is
  not a reliable way to reason about — or test — this issue.
- **Claude Code retried the refused `CONNECT` three times and took over two minutes** to
  surface the message. Fail-closed buys legibility, not speed, and nothing in the
  fallback can shorten it. The eventual message is precise; the wait is not short.

Claude Code also appends framing of its own — "This is a server-side issue, usually
temporary — try again in a moment" — so the body should not contradict it by claiming
the fault is local and permanent, and should be a single line with no trailing newline
(a trailing newline rendered as a stray `.` on its own line).

## Non-goals

- **Keeping traffic flowing.** The refusal is fail-closed by decision: traffic does not
  reach the internet unobserved. A fallback that tunnelled would preserve availability
  at the cost of the invariant that nothing egresses unseen, and would need a CA it must
  not have.
- **`curl` legibility.** It discards the response by design. Documentation is the only
  lever.
- **Covering `agentop service stop`.** The supervisor exits with the service, so the
  fallback is gone too. That case already prints an explicit warning.
- **Covering a unit that never loads.** Nothing of ours runs. `doctor` is the answer.
- **Renaming the lifecycle verbs.** Issue #1002's option B proposed renaming `stop` to
  mean "pause observation". That only pays off if the proxy keeps forwarding, which the
  fail-closed decision rules out, so the rename is cost with no benefit.
- **A second persistent service.** Issue #1002's option A proposed a shim always
  listening on the forward address with Cortex moved behind it. See *Alternatives*.
- **New configuration.** No flag, no config key.

## Design

`runSupervisor` (`cmd/cortex/supervise.go:162`) is today a re-exec loop: it filters the
supervise flag out of argv and restarts the child forever. It gains one more
responsibility — holding the forward address while no child is serving.

```
supervisor starts
  ├─ resolve forward address from argv's --config, else 127.0.0.1:47600
  ├─ open fallback listener                     ← nothing is serving yet
  loop:
  ├─ close fallback listener
  ├─ start child  ─────────────────────────────→ child binds the forward address
  ├─ wait for child to exit
  ├─ open fallback listener                     ← refuses, legibly, until the next child
  └─ sleep per restartPolicy, then loop
```

In steady state the fallback listener is closed, so a healthy install is unchanged: no
extra hop, no second process, no new port, no change to what the child does.

### The response

Written on the accepted socket, then the socket is closed:

```
HTTP/1.1 502 Cortex proxy is not serving
Content-Type: text/plain; charset=utf-8
Content-Length: <n>
Connection: close
Proxy-Connection: close

Cortex is routed in this environment (HTTPS_PROXY) but is not serving — run: agentop service start
```

The reason phrase serves Go clients, the body serves Claude Code. One line, no trailing
newline. The same bytes answer `CONNECT` and absolute-form plain-HTTP proxy requests:
the fallback's answer does not depend on the request, so it does not parse it beyond
reading the request line. It reads that one line rather than answering blind so the
client has finished writing before the close, which keeps the peer from seeing a reset
in place of the response it was about to read.

### Resolving the address

The supervisor has argv but has never parsed config. It gains a resolver that reads the
`--config` path for `listener.forward_proxy_addr` (`config.ListenerConfig.ForwardProxyAddr`
— note this is not agentop's `servicePaths.forwardAddr`, which is a different field) and
falls back to `127.0.0.1:47600`, the
fallback `setup_step_service.go:146` already uses for the same purpose. The fallback
path carries more weight than usual: a config that will not parse is one of the failure
modes this feature exists to cover, so the resolver must yield an address in exactly the
case the config is unreadable.

### Files

| File | Change |
|---|---|
| `cmd/cortex/fallback.go` | new — the listener, the response bytes, the address resolver |
| `cmd/cortex/supervise.go` | open/close the fallback around the child's lifetime |

No `core/` change, no agentop change, no config surface.

## Error handling

- **A fallback bind that fails** is logged at WARN and otherwise ignored. The
  supervisor's primary job is still to restart the child; losing the
  diagnostic listener must never stop that. The most likely cause is the previous child
  not having released the port yet, which the next iteration retries anyway.
- **The close-then-start race.** The supervisor closes its listener immediately before
  starting the child, so there is a window in which neither holds the port, and another
  in which the child's bind could lose to a straggler. The child's existing
  `exitPortInUse` path (#1366) already classifies that and the supervisor already
  dampens it; this design adds no new handling, it only must not make the window longer
  than one `Close()`.
- **Self-attribution.** `portconflict.go` reports a port's holder by pid. The supervisor
  must not report its own fallback listener as a foreign holder, which would turn this
  feature into a spurious port-conflict diagnosis. The fallback is closed before the
  child starts, so the child never sees it; the hazard is in any future code that probes
  the port from the supervisor side.
- **Connection storms.** Claude Code retries; other clients may retry harder. Each
  accepted connection is answered and closed immediately with no upstream work, and
  per-connection logging is DEBUG, so a retrying client cannot fill the log.

## Testing

| Test | Asserts |
|---|---|
| resolver: valid config | reads `listener.forward_proxy_addr` |
| resolver: unparseable config | falls back to `127.0.0.1:47600` |
| resolver: missing file, and no `--config` at all | falls back, no error |
| response bytes | exact status line, reason phrase, headers, single-line body with no trailing newline |
| Go client sees the reason phrase | a `net/http` client through the fallback fails with the reason phrase in the error — the regression that would silently break Go legibility |
| raw client sees the body | the body is present and is one line |
| lifecycle | fallback closed while a child runs, open once it exits |

The two client-visible tests matter most: the reason phrase and the body are each load-
bearing for a different stack, and either could be dropped by a well-meaning refactor
toward `http.Error` without any other test noticing.

## Alternatives

**A separate always-on shim service** (issue #1002's option A). A shim on the forward
address with Cortex behind it covers `agentop service stop` too, because it outlives the
supervisor. The costs are a second persistent launchd/systemd service to install,
supervise, version-stamp and uninstall; a new port for Cortex itself; an extra hop
through `CONNECT`, HTTP/2 and WebSocket upgrades on every request in the healthy case;
and a fresh "what if the shim dies" question one level up. For the cases the supervisor
fallback already covers this is a large amount of machinery, and the case it adds is the
one already covered by a printed warning.

Note the issue proposes moving Cortex to `:47602`, which is already the stats port
(`cmd/agentop/local_endpoint.go:234`, probed by `cmd/agentop/setup_probe.go:18`). The
pinned map is 47600 forward, 47601 session, 47602 stats, 47603 transparent, 47604
health, so option A would need a fresh port.

**Soft-fail into passthrough and rename the verbs** (issue #1002's option B). Ruled out
by the fail-closed decision, as above.

**Do nothing beyond diagnosis.** Argue the three existing mitigations suffice and spend
the effort on `doctor` distinguishing "supervisor up, proxy crash-looping" from "nothing
installed". Cheapest, and it adds no runtime surface, but it leaves the actual complaint
— an opaque error at the moment of failure — unaddressed.
