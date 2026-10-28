# Pi

[Pi](https://pi.dev) ([earendil-works/pi](https://github.com/earendil-works/pi)) is a
supported agent. Cortex recognises its User-Agent, gives each Pi session its own row, and
parses and prices its inference on the API formats Cortex reads. `agentop configure pi`
routes it through Cortex and takes that out again.

Two facts shape it:

- **The CA has to be in Pi's environment when it starts.** Pi is a Node program, and Node
  takes an extra CA only from `NODE_EXTRA_CA_CERTS`, read once at startup. Pi has no CA
  setting, and its `httpProxy` setting sets the proxy alone. So `enable` defines a `pi`
  shell function that starts Pi under `agentop exec`, the same approach as the `bob`
  shell function.
- **Pi sends no session id by default.** Without one, every Pi conversation lands in one
  bucket. Pi's extension API can add a header to every request, on every provider, so
  `enable` installs a small extension that sends Pi's session id as `x-session-id`.

## Enable and revert

```sh
agentop configure pi enable    # route Pi through Cortex
agentop configure pi status    # what is in place
agentop configure pi disable   # take it out again
```

Then open a new terminal and run `pi` as usual. It needs Cortex running.

**What `enable` writes**, shown together under one confirmation (`--yes` skips it):

| Piece | Where | What it does |
|---|---|---|
| Shell function | `~/.zshrc` or `~/.bashrc`, picked from `$SHELL` | `pi() { agentop exec -- pi "$@"; }` and `export CORTEX_PI=1`, between marker lines |
| Extension | `~/.pi/agent/extensions/cortex-session.ts`, or under `$PI_CODING_AGENT_DIR` | Sends Pi's session id as `x-session-id` |

The extension, as written:

```ts
export default function (pi: any) {
  pi.on("before_provider_headers", (event: any, ctx: any) => {
    const id = ctx.sessionManager?.getSessionId?.();
    if (id) event.headers["x-session-id"] = id;
  });
}
```

**Ownership.** Both pieces are fixed text, so agentop recognises them exactly:

- `enable` never overwrites. A file at the extension's path that agentop did not write is
  left alone and reported, and `enable` exits 1.
- `disable` removes only what `enable` wrote. An edited block, or one there twice, gets
  "remove it by hand between the markers" instead.
- An rc file reached through one symlink (a dotfiles repo) is edited at its target. A
  longer chain, or a shell other than zsh and bash, gets the block printed for you to
  place.

`agentop uninstall` removes both pieces by the same rules.

**`status`** says `enabled` when both pieces are in place, and `partly enabled` with the
missing piece named when only one is. It also says whether `CORTEX_PI` is set in the
current shell. If it is not, that terminal predates `enable`, so open a new one.

### Without the shell function

`agentop exec -- pi` does the same as the function for one run. Put the extension in place
by hand, or run `enable` and delete the block, to keep the session ids.

## What Cortex shows for it

- **Agent `pi`**, with the version from `pi/1.4.0 (darwin; node/v24.3.0; arm64)`. Pi has
  its own row in the Agents pane and its own spend, and `agentop server use <name> --agent
  pi` pins its new sessions to a server.
- **One session per Pi session**, titled from its first prompt. Its id is Pi's own session
  id, the one Pi's `/session` shows and its `--session-id` flag takes.
- **Inference** on Anthropic Messages, OpenAI Chat Completions and OpenAI Responses: model,
  messages, tool calls, token counts and cost.

Verified on Pi 1.4.0 with a LiteLLM gateway on `openai-completions`.

## Known issues

- **On an Anthropic subscription, Pi's traffic is filed under Claude Code.** Signed in to
  Anthropic with `/login` (Pro or Max), Pi sends `user-agent: claude-cli/<version>`.
  Cortex then counts that traffic and spend under Claude Code. Read from Pi's source
  (`pi-ai`'s `anthropic-messages.js`); not yet seen live. The session id still keeps each
  Pi session in its own row.
- **Some of Pi's APIs are proxied but not parsed:** `google-generative-ai`, `google-vertex`,
  `bedrock-converse-stream`, `mistral-conversations` and `pi-messages`. Requests go
  through, but no model, tokens or cost are recorded.
- **Tool results are not recorded on Chat Completions.** Tool calls are. This applies to
  every agent using that API.
- **No Timeline turns and no harvested titles.** Those come from Claude Code's own
  signals and transcripts, so Pi has host rows on the Timeline and a title from its
  traffic, the same as Bob, OpenCode and Codex.
- **A custom User-Agent is not recognised.** Pi's identity setting can replace its
  User-Agent. Its default and `short` forms are recognised; a custom string is not.
