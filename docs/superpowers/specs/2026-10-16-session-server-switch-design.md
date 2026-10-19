# Moving a running session to another inference server

Date: 2026-10-16
Status: proposed
Branch: `feat/session-switch` (spec and plan; implementation follows)

## Problem

1. An agent's inference server is chosen per agent (`agents.<agent>` in the
   inference-router's config, set with `agentop server use` or the TUI's `S` on
   the agents pane). The choice applies to **new** sessions only.
2. A running session is pinned to the server its first request reached, and
   nothing can change that pin: the session API only reads and deletes, the
   config names no session, and the sessions pane's SERVER cell is read-only.
   The only escapes offered today are "add the server back" and "start a new
   session".
3. That was deliberate. The 2026-10-06 design ruled out mid-conversation
   switches for three reasons: thinking-block signatures another server cannot
   verify, a lost prompt cache, and the client sizing requests for the model it
   believes it uses. None of the three still blocks a switch:
   - Measured (2026-10-16, direct calls to ete-int, ete-ext, glm and ollama,
     replaying real `/v1/messages`, `/v1/chat/completions` and `/v1/responses`
     bodies): every cross-server path continues the conversation correctly
     when the reasoning artifacts the producing server signed are dropped from
     the history. The client re-sends its whole history every turn, so nothing
     needs rebuilding.
   - The prompt cache is still lost on the first turn after a move. That is a
     cost the user must see before confirming, not a reason to refuse.
   - Window and output-cap mismatch is already handled by limit fitting
     (#1362), which runs on exactly these requests (`switched()`).
4. What is missing is a way to ask for the move, the one body rewrite that
   makes the moved history acceptable, and the visibility around both.

## Non-goals

- No automatic failover. A move is a user action (a keypress or a command);
  the router never guesses one (#1019 keeps that ambition for later, and
  builds on this).
- No wire-format translation. A moved request keeps its format; only the
  destination, path, key, model and limits change, as they already do for
  every routed request.
- No new session-API write endpoint. The unauthenticated API stays
  read/delete-only; the move travels through the config file, the same path
  `agentop server use` already takes.
- No handling of same-server cross-model signature strictness (ete-ext's
  opus-4-6 rejecting opus-5-5-signed blocks). That exposure exists today for
  any mixed-model routed session and is the model-substitution path's
  business, not the move's.

## Evidence the design rests on

All measured 2026-10-16 against the four configured servers, replaying bodies
one server produced to another — exactly what the router forwards after a
move. Probe scripts from the session that produced this spec: `/tmp/switch-probe/`
(`matrix.py`, `edge.py`, `crossfeed.py`, `resp_cross.py`).

| Format | Reasoning artifact | Crosses servers? |
|---|---|---|
| Anthropic `/v1/messages` | `thinking` block with `signature` | Sometimes: Bedrock-backed targets 400 a missing one; glm and ollama accept any. A Bedrock 403 on a foreign signature, measured 2026-10-16, did not reproduce 2026-10-17; Bedrock 4.6 then stopped thinking with a foreign block in history instead |
| Chat `/v1/chat/completions` | `reasoning_content`, `reasoning`, `thinking_blocks` | Always: every pairing 200, same server included |
| Responses `/v1/responses` | `reasoning` item with `encrypted_content` | Sometimes: vLLM-backed glm 400s any input reasoning item; ollama and ete-int accept |

Real traffic (597 Claude Code transcripts, 14 days) carries four block types
— `tool_use`, `tool_result`, `thinking`, `text` — so in the Anthropic format
`thinking` (with `redacted_thinking`, same family) is the only provider-bound
artifact a history holds.

The dividing line is **cryptographically-bound reasoning**: artifacts only
their producer can validate (a signature, an encrypted blob) are what fail;
plain-text reasoning fields never do. Anthropic-origin thinking text is empty
(encrypted reasoning), so dropping it loses nothing; glm and ollama thinking
text is readable, and dropping it loses that text, which the experiments
measured as harmless to continuation (a 4-turn glm tool loop with its thinking
stripped recalled the code word and completed every turn).

One more fact shapes the design: **the client's history is immutable.** The
router strips blocks from the request it forwards, but the client re-sends its
own unstripped history next turn, so a moved session carries the old server's
artifacts indefinitely.

## Design

### 1. The move: a `sessions:` entry in the router's config

The inference-router's config gains one field:

```yaml
inference-router:
  config:
    servers: { ... }
    agents:  { claude-code: glm }
    sessions:
      8e298f11-8bf1-4f40-a27f-ffef5876a5b9: ete-int   # a session id -> a server name
```

- The value is a server name, or `""` for "stop routing this session": the
  request goes where the client sent it, with the client's own credentials.
- An entry beats the session's stored pin and beats every `unpinned` rule,
  for as long as it is present. It is the user's explicit instruction; the
  router does not second-guess it.
- The pin follows: a request routed under an override settles the session's
  pin to the override's value (a new pin where none existed, a moved pin
  where another server was pinned, `pin: moved` in the invocation's details
  when it changed from a different server). Removing the entry later does not
  resurrect the old server.
- A session id with no such entry is decided exactly as today, rules
  unchanged and in their present order.
- Entries are inert once the pin matches them; they cost one line of config
  each. `agentop` prunes an entry when it writes the router's config and the
  session is neither live (`GET /v1/sessions`) nor held by the session
  archive's listing (`?archived=true`) — the same liveness test `x` and the
  erase paths use. A hand-written entry for a dead session is left alone
  until agentop next writes.

Validation (`routerconfig`): `sessions` is `map[string]string`; each key is
checked as a session id (a UUID-shaped string, or anything the existing
session ids look like — non-empty, printable, no whitespace); each value is
either `""` or a name listed under `servers`. A value naming no server is
refused, as `agents` is today, so a typo fails at write time, not first use.

Synthetic sessions (`default`, `pending:<agent>`) carry no override: their
ids hold many conversations, and moving one is meaningless. An override
naming a synthetic id is refused at validation.

An override names a session, not an agent: it applies to every request under
that session id. The pin it settles keeps its agent binding, as pins do
today.

### 2. What the router does with a moved request

Ordered, one addition to `serverFor`'s head:

1. Not redirectable — unchanged, before anything else.
2. The session's id has a `sessions:` entry → that entry's value is the
   server (or "" for unrouted). The pin is settled to it, whatever it was.
   The request proceeds through the existing redirect path: key, path
   rewrite, model choice, limits. Removed-server check applies to the
   override's server too, with the same 503 and text.
3. No entry → today's rules, unchanged.

The invocation records a move: `routed` with `pin: moved` and the server
named, so the timeline shows where the conversation went and that it was
moved there rather than born there.

### 3. The strip: drop producer-bound reasoning from a switched request

One rule, format-level, no per-server knowledge:

**A request the router is sending to a server other than the one that
produced its reasoning has that reasoning dropped — the artifacts only their
producer can validate. Plain-text reasoning is never touched.**

Per format, what is dropped from prior turns (all assistant turns; a request
holds only history):

- **Anthropic messages**: `thinking` and `redacted_thinking` blocks. If an
  assistant message is left with no blocks, the whole message is dropped —
  safe, because a thinking-only turn carries no `tool_use`, so no
  `tool_result` references it.
- **Responses**: `reasoning` items. Every other item type is kept.
- **Chat completions**: nothing. Measured: nothing in this format fails
  cross-server, so a strip would be pure loss.

Provenance, revised 2026-10-17. A reasoning artifact is readable only by the
server that wrote it, so each request of a moved session keeps the artifacts its
destination wrote and drops the rest, before it is sent. One request stays one
request, and a session moved back gets its first server's reasoning back. The
first design stripped every artifact on every request after a move, the new
server's own included; a second draft dropped only on a refusal and resent,
which made every refused request two.

- **Who wrote an artifact.** An artifact first appears in the request that
  follows its writer's answer, the next request of the same conversation (the
  router's `conversationKey`, so a subagent and a one-shot are their own). So
  its writer is where that conversation's previous request went. The router
  keeps, per moved session, each artifact's fingerprint to its writer, and each
  conversation's latest destination (`inference-router/reasoning/<session>`, the
  pin's TTL, renewed with the pin). Responses are never read, so streamed
  answers need nothing.
- **When it starts.** On the request that moves the session, from a pin or from
  none: every artifact before the move is the server the session ran on
  then. A session that never moves records nothing and drops nothing.
- **What it covers.** Requests the router routes and requests it leaves
  unrouted (a move to `own`, and an agent the override alone routed), so a move
  to the client's own provider is covered. A fingerprint is the signature, a
  redacted block's data, or a reasoning item's id and content; an unsigned block
  (Ollama) adds its conversation and position, since two can share a text.

Not done, on purpose:

- No error-text matching, no learn-and-resend for reasoning: a resend makes one
  request two. The signals are
  unusable: Bedrock's foreign-thinking 403 is AWS SigV4 signing language,
  indistinguishable from a real auth failure; vLLM's is a 157-error blob.
- No signature-shape heuristics ("long base64 is Anthropic's"). The
  provenance rule needs none, and an unknown server costs at most a stripped
  reasoning block, never a hard failure.

### 4. agentop: `S` on a session row, `agentop server move`

The sessions pane's `S` opens the server picker on the highlighted session —
the same overlay component the agents pane uses, titled for the session, not
the agent. Options: every configured server, plus "own provider (unrouted)"
for `""`. `S` stays per-agent on the agents pane; the sessions pane's cell
comment ("Read-only ... that would move a running conversation, which the
router exists not to do") is replaced by the picker's own text.

Before the move applies, a confirmation panel states the cost, from facts the
session API already serves:

- the session's `promptContext` (its live context size) re-sent uncached to
  the chosen server on the first turn there — shown as a token count;
- a warning when the chosen server is a different one from the session's
  current server (a same-server pick is a no-op and is not offered);
- nothing about model names: the substitute machinery already handles a
  refusal with `main`/`helper` and keeps the substitute per session.

The write path is the one `agentop server use` uses: `edit.WritePluginConfig`
with `Path: ["sessions", <id>]`, `servers.Verify` extended to check the
`sessions` map, a reload poll of `/reload/status`, and a rollback of the
file on refusal.

The CLI gains the same action: `agentop server move <session-id> <server>`
and `agentop server move <session-id> ""` (or `own`), with the same
confirmation text and the same running-session conventions as `use`/`reset`
(printed, not interactive-blocking).

### 5. What is recorded, where it is documented

- Invocations: `routed` with `pin: moved` on the first request under an
  override that changed the server; `reasoning_stripped {blocks: N}` on
  each request whose other servers' reasoning was dropped. Both render in agentop's timeline
  without new vocabulary (both are `modify`).
- The plugin store gains one key family: `inference-router/reasoning/<session>`
  (value: each reasoning artifact's fingerprint to the server that wrote it, and
  each conversation's latest destination), for moved sessions only. It joins pins, refusals,
  substitutes, catalogs, limits and cuts in `~/.cortex/plugin-state.json`;
  `X` does not clear it, and it expires with the pin's TTL.
- Docs: `docs/inference-router-plugin.md` (the sessions field, the strip,
  the moved pin), `docs/agents/claude-code.md` (moving a running session),
  `docs/plugin-catalog.md`, `cmd/agentop/README.md` (the `move` action and
  the sessions-pane `S`), `docs/laptop-service.md` (the reasoning key in the
  plugin-state paragraph).

## Testing

- Unit (routerconfig): sessions decode, validation refusals (unknown
  server, synthetic id, malformed id), lenient read.
- Unit (inferencerouter): the override beats pin and `unpinned`; the pin
  settles to the override; `pin: moved` recorded; removed-server 503 on an
  override's server; strip per format (thinking blocks, redacted_thinking,
  empty-message drop, responses reasoning items, chat untouched); a session
  that never moves keeps everything; a move drops the old server's artifacts and
  keeps the new server's; a move back keeps the first server's; side requests,
  pinless sessions and `own` are covered; synthetic sessions never
  overridden.
- Integration (forwardproxy, the `inferencerouter_test.go` pattern): a
  session running on server A, config gains a `sessions:` entry for B, next
  request goes to B with B's key and A's reasoning dropped, one request per turn; the timeline shows
  the move; a second request stays on B.
- agentop: the edit write/read of a sessions entry; the prune rule; TUI `S`
  on a session row applies to that session only.

## Known gaps left open

- A hand-off to a new session id (continued-in, `--fork-session`, `/clear`
  with running background subagents) carries the conversation to an id no
  override follows, exactly as no pin follows it today.
- The pins-by-name rename hazard (renaming a server 503s pinned sessions) is
  unchanged and now also applies to `sessions:` entries by the same
  mechanism.
- The prompt-cache cost of a move is shown, not avoided.
