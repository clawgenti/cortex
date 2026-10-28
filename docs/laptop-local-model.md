# Run an agent on a local model

When an inference provider is out — a dead key, a rate limit, an outage — or
when you would rather the traffic never leave the machine, Cortex can send an
agent's new sessions to a model server running locally. This page walks the
whole path: standing the server up, adding it to Cortex, and what changes for
the agent.

The reference setup is [Ollama](https://ollama.com) running `granite4.2:8b`:
an 8.8B model that calls tools, thinks before answering, and runs on a laptop.
Any OpenAI- or Anthropic-compatible server works the same way — LM Studio,
llama.cpp's `llama-server`, vLLM — and is added with the same two commands;
[Local model servers](../cmd/agentop/README.md#local-model-servers) covers
their specifics.

Two things to know before starting:

- **A local model is a lesser model.** `granite4.2:8b` follows instructions
  and calls tools reliably, but it is not a frontier model: expect more
  retries and wrong turns on hard work. It is a fallback that keeps you
  working — and one whose traffic stays on your machine — not a replacement.
- **Only new sessions move.** A conversation already running keeps the server
  it started on, wherever the agent addressed its requests. Switch before
  starting new work, not in the middle of it.

## Stand up the server

| | |
|---|---|
| macOS | the app from [ollama.com/download](https://ollama.com/download) |
| Linux | `curl -fsSL https://ollama.com/install.sh \| sh` |

Start Ollama — the macOS app does this at login, and on Linux the installer
starts an `ollama` service (where there is no systemd, run `ollama serve`) —
and pull the model:

```sh
ollama pull granite4.2:8b      # a 5.3 GB download
```

Check that it answers:

```sh
curl -s localhost:11434/v1/models | jq -r '.data[].id'
```

`granite4.2:8b` should be on the list. Ollama serves both dialects an agent
sends — the OpenAI chat-completions format and the Anthropic Messages format
— so Claude Code and OpenAI-dialect agents route to it alike, with no
translation gateway between.

### Context length

This is the one setting that matters. Ollama does not refuse a prompt too
large for its window: it trims it silently. And a fresh install does not give
a model its own window. It picks one from the GPU memory it finds — **4k
below about 24 GB, 32k below about 48 GB**, the model's full window only
above that. On Apple silicon that is part of the unified memory: a 64 GB Mac
reports 51.8 GB. Claude Code's opening turn can pass 64k tokens, so routed
to an Ollama that picked 4k or 32k its instructions would be cut down with
no error anywhere — the agent would simply behave strangely, and nothing
would say why.

Set the window to the model's full 131k where the server is started:

- **The macOS app:** Settings → Context length → 128k, the slider's name for
  131072. The app restarts its server when the value changes. Use the slider
  rather than `launchctl setenv`: the app overrides `OLLAMA_CONTEXT_LENGTH`
  with the slider's value once the slider has been set, and `launchctl
  setenv` does not survive a reboot.
- **The Linux service the installer set up:** it already holds port 11434,
  so a second `ollama serve` cannot start. Set the variable on the service:
  run `sudo systemctl edit ollama.service`, add

  ```ini
  [Service]
  Environment="OLLAMA_CONTEXT_LENGTH=131072"
  ```

  where the editor says, save, then `sudo systemctl restart ollama`.
- **A server run from a shell:** `OLLAMA_CONTEXT_LENGTH=131072 ollama serve`.

Then check what actually loaded. With the model running, `ollama ps` shows
the window in its CONTEXT column — that is the truth, whatever was
configured:

```
NAME             SIZE     CONTEXT
granite4.2:8b    27 GB    131072
```

The cost is memory: the cache grows with the window, and `granite4.2:8b`
measured 27 GB resident at full context. On a Mac with less than 64 GB, pick
the window and the model together — see the next section.

### Fit the model to your memory

On a Mac with less than 64 GB, `granite4.2:8b` can be slow. A 36 GB Mac runs
it slowly at 64k and stalls at 128k.

The model itself is small. What grows is the cache that holds the context:
the bigger the window, the more memory it takes. When it no longer fits in
GPU memory, the Mac swaps and everything slows down.

**Pick a model and window by memory:**

| Your Mac's memory | Model | Window for Bob | Window for Claude Code |
|---|---|---|---|
| 64 GB or more | `granite4.2:8b` | 32k | 128k |
| 32–48 GB | `granite4.2:3b` | 32k | 64k |
| Under 32 GB | `granite4:tiny-h` | 32k | 64k |

**Pick the window by agent.** The window must hold the agent's largest
prompt. A bigger window only costs memory and time.

| Agent | Largest prompt seen | Window |
|---|---|---|
| Bob | about 21k tokens | 32k |
| Claude Code | over 64k tokens | 64k or more |

**Memory each model uses** (GiB):

| Model | 32k | 64k | 128k |
|---|---|---|---|
| `granite4.2:8b` | 9.7 | 14.9 | 25.4 |
| `granite4.2:3b` | 4.4 | 7.2 | 12.1 |
| `granite4:tiny-h` | 4.4 | 4.9 | 5.8 |

**Speed** on a 20k-token prompt, the size of a Bob prompt:

| Model | Wait for the first word | Writing speed |
|---|---|---|
| `granite4.2:8b` | 48 s | 45 tokens/s |
| `granite4.2:3b` | 23 s | 82 tokens/s |
| `granite4:tiny-h` | 8.5 s | 124 tokens/s |

The wait happens once, at the start of a conversation. Later turns start in
about 0.1 s. These numbers come from an M4 Max with Ollama 0.40.5; a smaller
chip is slower.

**What you give up:**

| Model | Good | Not so good |
|---|---|---|
| `granite4.2:8b` | Best answers | Needs the most memory |
| `granite4.2:3b` | Thinks before answering; twice as fast as 8b | Weaker answers than 8b |
| `granite4:tiny-h` | Fastest; memory hardly grows with the window | No thinking; older Granite 4.0; not yet tried with an agent end to end |

`granite4:tiny-h` cannot think, and Ollama refuses a request that asks it to
(`"granite4:tiny-h" does not support thinking`). If you see that error, use
`granite4.2:3b` instead.

**Check that it fits.** Run `ollama ps` while the model is loaded. The
PROCESSOR column should say `100% GPU`. Any CPU share means the model did not
fit and will be slow. Leave room for macOS, the agent and your editor.

**Save a little more memory.** Setting `OLLAMA_KV_CACHE_TYPE=q8_0` where the
server starts, the same way as the context length above, shrinks the cache.
It cuts `granite4.2:8b` to 7.4 GiB at 32k and 15.7 GiB at 128k. It does not
make anything faster. The macOS app has no setting for it.

**Change the model.** Pull it, then replace the server entry with `--yes`:

```sh
ollama pull granite4.2:3b
echo ollama | agentop server add ollama http://localhost:11434 \
  --main granite4.2:3b --helper granite4.2:3b --key-stdin --yes
```

Then start a new session. Sessions already running keep the old model.

### Thinking

The model thinks before answering, on by default, and Ollama keeps the
thinking out of the answer: an Anthropic-format reply carries it as separate
`thinking` blocks, an OpenAI one as a `reasoning` field. Leave it on. The
separation is why the reference model is granite **4.2** — granite3.3, the
previous release, leaks its reasoning into the answer, and an agent reading
that answer gets the model's deliberations mixed into its work.

## Route the agent to it

Two commands, and nothing restarts:

```sh
echo ollama | agentop server add ollama http://localhost:11434 \
  --main granite4.2:8b --helper granite4.2:8b --key-stdin
agentop server use ollama --agent claude-code
```

- Ollama checks no key, but `add` insists on one, so `echo ollama |` supplies
  a placeholder; it is read from stdin, so it never lands in shell history.
- `--main` and `--helper` are model words. `add` reads the server's model
  list and refuses a word that names none of its models — which is also what
  catches a wrong URL, or a server that is not running. A word names the
  newest model whose name contains it, judged by the numbers in the name, so
  give the full tag: a bare `granite` names granite3.3 if that is pulled too.
  Even the full tag only matches, and pulling another tag of the model, such
  as `granite4.2:8b-q8_0`, can move it there. `add` prints what each word
  names — `main granite4.2:8b → granite4.2:8b` — and `agentop server` shows
  it again at any time. `main` is sent for a request with tools, the agent's
  main work; `helper` for one without, such as a title.
- `--agent` takes the name agentop shows the agent — `agentop configure
  status` lists yours. In the TUI, `S` on the agents pane makes the same
  choice.

The agent needs no setting of its own beyond going through Cortex. The full
command reference — removing a server, rotating a key, what each refusal
means — is [Choosing an inference
server](../cmd/agentop/README.md#choosing-an-inference-server-agentop-server).

## What happens next

- **New sessions only.** A session is pinned to the server its first request
  reached and stays there, and the pin survives a proxy restart. Start a new
  `claude` after switching — and switch before a `/clear`, not after: the
  conversation `/clear` begins is what gets pinned.
- **The model.** The agent keeps asking for its own model names — Claude
  Code's, say — which the local server does not have, and refuses. Before
  the agent sees the refusal, the router sends the request again with the
  word's model: one refused round trip, then the substitute is remembered
  for an hour and sent from the start, and the session keeps the model it
  was given.
- **Where it shows.** agentop's agents pane carries a `SERVER` column naming
  the server each agent's new sessions go to. The sessions table gains one,
  naming where each session's inference went, once more than one server is
  configured and where the terminal has room for it. The detail pane's
  `redirected:` and `model:` lines name the host and the model the agent
  asked for when they were others.
- **What it costs.** Tokens are counted as for any server, and the model
  reports unpriced until you give it a rate ([pricing](pricing.md)) — a
  local model's marginal cost is electricity.
- **Switching back.** `agentop server use <name> --agent <agent>` routes new
  sessions to the other server; conversations running on the local model
  finish where they are.
- **Nothing fails over.** If the local server is down, its sessions' requests
  fail. Route the agent elsewhere and start a new session; Cortex moves no
  conversation on its own.

## Picking a different model

The reference model was picked by criteria, not brand. Check any candidate
the same way:

1. **Tool use** — the make-or-break: a model that cannot call tools cannot
   drive a coding agent. `ollama show <model>` lists it under capabilities.
2. **Separated thinking** — reasoning that leaks into the answer confuses the
   agent reading it. Ask the model something trivial and check the thinking
   arrives in its own field, not in the answer.
3. **Context window** — at least the agent's largest turn; see
   [Context length](#context-length).
4. **Memory** — the weights, plus the cache at the window you run. The pull
   size is only the weights; `ollama ps` shows the real figure. See
   [Fit the model to your memory](#fit-the-model-to-your-memory).

Then give it a small real task through the agent before trusting it with a
large one.

## See also

- [Choosing an inference server](../cmd/agentop/README.md#choosing-an-inference-server-agentop-server) —
  every `agentop server` command, and the refusal wordings it reads
- [Local model servers](../cmd/agentop/README.md#local-model-servers) —
  vLLM, llama.cpp and LM Studio specifics
- [The inference-router](inference-router-plugin.md) — the routing rules,
  pins and substitution in full
- [Your agent's guide](agents/) — each has a switching section with its
  agent's own caveats
- [Pricing](pricing.md) — rates, and what reports unpriced
