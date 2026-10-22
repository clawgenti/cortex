# Script plugins

Customize the Cortex running on your laptop with a small script, without
writing a Go plugin or rebuilding anything. A script can read, change or block
each request the agent sends, and keeps its own state between requests.

Go plugins remain the way to build anything permanent, shared, or on the hot
path. Script plugins are for the quick, personal customization.

## Write one

Put an executable file in `~/.cortex/plugins/`:

```python
#!/usr/bin/env python3
# ~/.cortex/plugins/10-secrets.py
import re
from cortex import on, run

SECRETS = re.compile(r"\bAKIA[0-9A-Z]{16}\b|\bgh[pousr]_[A-Za-z0-9]{36,}\b")

@on.request
def mask(flow):
    flow.replace_text(SECRETS, "[secret]")

@on.request
def no_force_push(flow):
    if "git push --force" in flow.latest_text:
        flow.deny("force-push needs a human")

run()                 # shadow mode: records what it would do, changes nothing
```

```sh
chmod +x ~/.cortex/plugins/10-secrets.py
```

Cortex starts it within a second, in **shadow mode**. That is the whole
installation: no config edit, no restart. `from cortex import ...` resolves to
`~/.cortex/plugins/_sdk/cortex.py`, which Cortex keeps up to date itself.

## Test it on what you already sent

Every request your agents sent is in a session Cortex recorded. Replay one
through the script before letting it touch anything:

```sh
PYTHONPATH=~/.cortex/plugins/_sdk ~/.cortex/plugins/10-secrets.py --replay <session-id>
```

```
#663  modify  ' paste `deploy with AKIAIOSFODNN7EXAMPLE please` i' -> ' paste `deploy with [secret] please` into Claude C'
replayed 305 requests: 217 modified, 0 denied, 0 errors, 88 unchanged
```

Session ids are in agentop's sessions pane. Replay rebuilds each request from
the prompt text Cortex recorded, so it covers hooks that work on text —
`replace_text`, `latest_text`, `texts` — and not ones that depend on exact body
bytes or headers.

## Watch it, then turn it on

In agentop each script has its own row on every request it ran on, named after
its file (`10-secrets.py` is `secrets`): `modify (shadow)` for an edit it would
have made, `deny (shadow)` for a request it would have blocked. Anything the
script `print`s appears on that row.

When it does what you want, change the last line to:

```python
run(enforce=True)
```

and save. Cortex restarts that one script; from the next request its edits and
denies apply.

## Turn it off

Delete the file, or rename it to start with `_` (`_10-secrets.py`). Cortex stops
it within a second.

## The `flow`

| | |
|---|---|
| `flow.session` | the session id |
| `flow.host`, `flow.path`, `flow.method` | where the request is going |
| `flow.headers` | request headers, lower-case. Credentials (`authorization`, `x-api-key`, cookies) are never included |
| `flow.body`, `flow.json` | the body as text, and parsed (or `None`). Changes made to `flow.json` are not sent — call `flow.set_body(flow.json)` |
| `flow.texts` | every prompt text fragment: system prompt, messages, tool results |
| `flow.latest_text` | the newest user message only |
| `flow.inference` | what Cortex's parser found: `model`, `tools`, `messages`, … |
| `flow.replace_text(pattern, repl)` | rewrite every prompt text fragment; returns how many changed |
| `flow.set_body(body)` | replace the body outright |
| `flow.set_header(name, value)`, `flow.remove_header(name)` | edit headers |
| `flow.deny(reason, status=403)` | block the request |
| `flow.record(action, reason, **details)` | say what the hook did, for its row |

`replace_text` and `texts` understand Anthropic Messages, OpenAI Chat
Completions and OpenAI Responses bodies. Other formats are in `flow.json`.

## Things worth knowing

- **Gate on `latest_text`, not `texts`.** An agent re-sends the whole
  conversation on every turn. A rule that denies on anything in the history
  denies every later turn too, until the conversation is rewound.
- **Make replacements deterministic.** The provider caches the conversation's
  prefix. A replacement that differs from turn to turn (a random token, a
  timestamp) misses that cache on every request. Turning a rewrite on in the
  middle of a session misses it once.
- **State is in memory.** Module-level variables keep their values between
  requests while the file is unchanged, and across edits to Cortex's config.
  Editing the script, a crash, or restarting Cortex starts it fresh. Write to a
  file if it must survive those.
- **One request at a time.** Each script answers one request before the next,
  so its state needs no locking. A script that takes longer than the slot's
  `timeout` (2s) is restarted, and the request goes through.
- **A broken script does not break your agent.** An exception, a crash or a
  timeout is shown on the request's row and the request continues. To block
  instead, see `fail_closed` below.
- **What Cortex records is what the agent sent.** Cortex's parsers read the
  request before any script runs, so the session view and archive keep the
  original text, even where a script masked it on the wire.
- **Scripts run as you,** with your login shell's `PATH`, and only on a local
  install. A sidecar with the same config runs none of them.
- **Your script's own HTTP calls do not go through Cortex.** Proxy variables
  are removed from its environment, so it cannot loop back into itself.

## Order, and running between Go plugins

Scripts run in file-name order; a number prefix (`10-`, `20-`) sets it and is
not part of the name. Each sees the request as the one before it left it. A
script in shadow mode changes nothing, so the next one sees what it saw.

All of `~/.cortex/plugins/` is one slot, which Cortex appends to the end of the
outbound pipeline. To put scripts somewhere else — between two Go plugins, for
example — list the slot yourself and give it a folder of its own:

```yaml
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: script
        config:
          dir: early            # ~/.cortex/plugins/early/
      - name: tool-prune
      - name: script            # ~/.cortex/plugins/ itself
```

Once the config names `script` anywhere, Cortex appends nothing. A slot must
come after the parsers: it rewrites requests, and readers go first.

| Field | Default | Meaning |
|---|---|---|
| `dir` | (empty) | Folder inside `~/.cortex/plugins` this slot runs |
| `timeout` | `2s` | How long one script may take to answer one request |
| `fail_closed` | `false` | Block a request when an enforcing script fails on it |

## Without the SDK

The SDK is a convenience; the protocol is one JSON object per line on stdin and
stdout, so any language works. A script first prints its handshake, then answers
each message with the `id` it carried:

```sh
#!/bin/sh
# ~/.cortex/plugins/tag.sh — needs jq
echo '{"cortex":1,"hooks":["request"],"enforce":true}'
while IFS= read -r line; do
  printf '%s' "$line" | jq -c '{id: .id, request: {set_headers: {"X-Tagged": "yes"}}}'
done
```

A message carries `hook`, `session` and `request` (`method`, `host`, `path`,
`headers`, `body` or `body_base64`, and the parsers' `inference` / `mcp` /
`a2a`). A reply may carry `deny` (`reason`, `status`, `code`), `request`
(`body` or `body_base64`, `set_headers`, `remove_headers`), and `invocation`
(`action`, `reason`, `details`). An empty reply, `{"id": N}`, means continue
unchanged. Any line that is not a reply is treated as the script's output and
shown on the row.
