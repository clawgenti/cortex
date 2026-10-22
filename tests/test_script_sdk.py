"""Tests for the script-plugin SDK, core/plugins/script/sdk/cortex.py."""

import importlib.util
import io
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

REPO = pathlib.Path(__file__).resolve().parent.parent
SDK = REPO / "core" / "plugins" / "script" / "sdk" / "cortex.py"
GUIDE = REPO / "docs" / "script-plugin.md"


@pytest.fixture
def cortex():
    """A fresh copy of the module per test: hooks register on a module global."""
    spec = importlib.util.spec_from_file_location("cortex", SDK)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def anthropic_request(*user_texts, system="You are helpful."):
    body = {
        "model": "claude-opus-5-5",
        "system": [{"type": "text", "text": system}],
        "messages": [{"role": "user", "content": [{"type": "text", "text": t}]} for t in user_texts],
    }
    return {
        "hook": "request",
        "session": {"id": "s1"},
        "request": {"host": "api.anthropic.com", "body": json.dumps(body)},
    }


def openai_request(*user_texts):
    body = {"model": "gpt-5", "messages": [{"role": "system", "content": "sys"}]}
    body["messages"] += [{"role": "user", "content": t} for t in user_texts]
    return {"hook": "request", "session": {"id": "s1"}, "request": {"host": "api.openai.com", "body": json.dumps(body)}}


def test_replace_text_rewrites_every_fragment_anthropic(cortex):
    @cortex.on.request
    def mask(flow):
        flow.replace_text(r"AKIA[0-9A-Z]{16}", "[key]")

    reply, _ = cortex.handle(anthropic_request("use AKIAIOSFODNN7EXAMPLE", "and AKIAIOSFODNN7EXAMPLE again"))
    body = json.loads(reply["request"]["body"])
    assert [m["content"][0]["text"] for m in body["messages"]] == ["use [key]", "and [key] again"]
    assert body["system"][0]["text"] == "You are helpful."


def test_replace_text_openai_string_content(cortex):
    @cortex.on.request
    def mask(flow):
        flow.replace_text("secret", "***")

    reply, _ = cortex.handle(openai_request("a secret"))
    body = json.loads(reply["request"]["body"])
    assert body["messages"][1]["content"] == "a ***"


def test_tool_results_are_text_too(cortex):
    @cortex.on.request
    def mask(flow):
        flow.replace_text("secret", "***")

    msg = anthropic_request("x")
    body = json.loads(msg["request"]["body"])
    body["messages"].append(
        {
            "role": "user",
            "content": [
                {"type": "tool_result", "tool_use_id": "t1", "content": [{"type": "text", "text": "a secret file"}]},
                {"type": "image", "source": {"data": "secret"}},  # not text: left alone
            ],
        }
    )
    msg["request"]["body"] = json.dumps(body)
    reply, _ = cortex.handle(msg)
    content = json.loads(reply["request"]["body"])["messages"][-1]["content"]
    assert content[0]["content"][0]["text"] == "a *** file"
    assert content[1]["source"]["data"] == "secret"


def test_an_unchanged_request_sends_no_body(cortex):
    @cortex.on.request
    def mask(flow):
        flow.replace_text("nothing-matches-this", "x")

    reply, _ = cortex.handle(anthropic_request("hello"))
    assert reply == {}


def session_api_answer(events, url):
    """What Cortex's session API answers for url over events: a page of at most limit
    events ending before ?before, totalEvents and oldestSeq when the page is not the
    whole session, ?view=summary without message bodies, and one event by seq."""
    u = urllib.parse.urlsplit(url)
    q = dict(urllib.parse.parse_qsl(u.query))
    parts = u.path.strip("/").split("/")
    held = sorted(events, key=lambda e: e["seq"])
    if len(parts) == 5 and parts[3] == "events":
        for e in held:
            if e["seq"] == int(parts[4]):
                return e
        raise KeyError("no event %s" % parts[4])
    before = int(q.get("before", 0))
    limit = min(int(q.get("limit", 500)), 2000)
    end = len(held)
    if before:
        end = next((i for i, e in enumerate(held) if e["seq"] >= before), len(held))
    start = max(0, end - limit)
    page = held[start:end]
    if q.get("view") == "summary":
        page = [summarized(e) for e in page]
    view = {"events": page}
    if start > 0 or end < len(held):
        view["totalEvents"], view["oldestSeq"] = len(held), held[0]["seq"]
    return view


def summarized(e):
    e = dict(e)
    if e.get("inference"):
        inf = dict(e["inference"])
        n = len(inf.pop("messages", None) or [])
        inf.pop("toolResults", None)
        if n:
            inf["messageCount"] = n
        e["inference"] = inf
    return e


def fake_api(cortex, monkeypatch, events):
    monkeypatch.setattr(cortex, "_get", lambda url: session_api_answer(events, url))


def recorded(seq, *messages, tool_results=None):
    """A recorded inference request, as the session API serves it."""
    inf = {"model": "m", "messages": [{"role": r, "content": c} if c else {"role": r} for r, c in messages]}
    if tool_results:
        inf["toolResults"] = tool_results
    return {"seq": seq, "phase": "request", "sessionId": "s", "host": "h", "inference": inf}


def test_latest_text_is_only_the_newest_user_message(cortex):
    seen = []

    @cortex.on.request
    def gate(flow):
        seen.append(flow.latest_text)

    cortex.handle(anthropic_request("first rm -rf /", "second"))
    assert seen == ["second"]


def tool_turn(*content):
    """An Anthropic request whose newest user message is the given blocks, after
    the user asked something and the agent called a tool."""
    msg = anthropic_request("please tidy the repo")
    body = json.loads(msg["request"]["body"])
    body["messages"].append({"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "Read"}]})
    body["messages"].append({"role": "user", "content": list(content)})
    msg["request"]["body"] = json.dumps(body)
    return msg


def tool_turn_with(content):
    """tool_turn, with the newest user message's content a value rather than blocks."""
    msg = tool_turn()
    body = json.loads(msg["request"]["body"])
    body["messages"][-1]["content"] = content
    msg["request"]["body"] = json.dumps(body)
    return msg


TOOL_RESULT = {"type": "tool_result", "tool_use_id": "t1", "content": "never git push --force"}
TOOL_RESULT_BLOCKS = {
    "type": "tool_result",
    "tool_use_id": "t1",
    "content": [{"type": "text", "text": "never git push --force"}],
}


@pytest.mark.parametrize(
    "content, want",
    [
        # A tool result is not the user's words, whether its content is blocks or a string.
        ([TOOL_RESULT_BLOCKS], "please tidy the repo"),
        ([TOOL_RESULT], "please tidy the repo"),
        ([TOOL_RESULT, TOOL_RESULT], "please tidy the repo"),
        # Text beside a tool result is.
        ([TOOL_RESULT, {"type": "text", "text": "now commit"}], "now commit"),
        # A message with no content at all, as replay rebuilds one without text.
        ([], "please tidy the repo"),
        # A user message that is not a tool result ends the search, text or not.
        ([{"type": "image", "source": {"data": "x"}}], ""),
        ([TOOL_RESULT, {"type": "image", "source": {"data": "x"}}], ""),
        ([{"type": "text", "text": ""}], ""),
        ("", ""),
        (None, ""),
    ],
)
def test_latest_text_is_not_tool_output_anthropic(cortex, content, want):
    seen = []

    @cortex.on.request
    def gate(flow):
        seen.append(flow.latest_text)

    cortex.handle(tool_turn(*content) if isinstance(content, list) else tool_turn_with(content))
    assert seen == [want]


def test_latest_text_is_not_tool_output_openai(cortex):
    seen = []

    @cortex.on.request
    def gate(flow):
        seen.append(flow.latest_text)

    msg = openai_request("please tidy the repo")
    body = json.loads(msg["request"]["body"])
    body["messages"].append({"role": "assistant", "tool_calls": [{"id": "t1", "type": "function"}]})
    body["messages"].append({"role": "tool", "tool_call_id": "t1", "content": "never git push --force"})
    msg["request"]["body"] = json.dumps(body)
    cortex.handle(msg)
    assert seen == ["please tidy the repo"]


def test_latest_text_is_empty_without_user_text(cortex):
    seen = []

    @cortex.on.request
    def gate(flow):
        seen.append(flow.latest_text)

    msg = anthropic_request()
    body = json.loads(msg["request"]["body"])
    body["messages"] = [{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "out"}]}]
    msg["request"]["body"] = json.dumps(body)
    cortex.handle(msg)
    assert seen == [""]


def test_the_guides_gate_ignores_tool_output(cortex):
    @cortex.on.request
    def no_force_push(flow):
        if "git push --force" in flow.latest_text:
            flow.deny("force-push needs a human")

    reply, _ = cortex.handle(tool_turn(TOOL_RESULT))
    assert "deny" not in reply


# Replay rebuilds a tool turn from the parse, which keeps no tool result: the hook
# must read the same latest_text there as it would live.
def test_replay_reads_latest_text_as_live(cortex, monkeypatch):
    seen = []

    @cortex.on.request
    def gate(flow):
        seen.append(flow.latest_text)

    events = [recorded(1, ("user", "please tidy the repo"), ("assistant", ""), ("user", ""))]
    fake_api(cortex, monkeypatch, events)
    cortex.replay("s", write=lambda s: None)
    live = tool_turn({"type": "tool_result", "tool_use_id": "t1", "content": "out"})
    cortex.handle(live)
    assert seen == ["please tidy the repo", "please tidy the repo"]


def test_deny(cortex):
    @cortex.on.request
    def gate(flow):
        if "rm -rf" in flow.latest_text:
            flow.deny("destructive", status=451)

    reply, _ = cortex.handle(anthropic_request("please rm -rf /"))
    assert reply == {"deny": {"reason": "destructive", "status": 451, "code": "policy.forbidden"}}


def test_headers_and_record(cortex):
    @cortex.on.request
    def tag(flow):
        flow.set_header("X-Team", "a")
        flow.remove_header("X-Debug")
        flow.record("observe", "tagged", team="a")

    reply, _ = cortex.handle(anthropic_request("x"))
    assert reply["request"] == {"set_headers": {"X-Team": "a"}, "remove_headers": ["X-Debug"]}
    assert reply["invocation"] == {"action": "observe", "reason": "tagged", "details": {"team": "a"}}


def test_print_is_captured_and_exceptions_reported(cortex):
    @cortex.on.request
    def noisy(flow):
        print("looking at", flow.host)
        raise ValueError("boom")

    reply, printed = cortex.handle(anthropic_request("x"))
    assert printed == "looking at api.anthropic.com"
    assert "ValueError: boom" in reply["error"]


def test_state_persists_between_requests(cortex):
    count = {}

    @cortex.on.request
    def counter(flow):
        count[flow.session] = count.get(flow.session, 0) + 1
        if count[flow.session] > 2:
            flow.deny("too many")

    assert "deny" not in cortex.handle(anthropic_request("a"))[0]
    assert "deny" not in cortex.handle(anthropic_request("b"))[0]
    assert "deny" in cortex.handle(anthropic_request("c"))[0]


def test_run_speaks_the_protocol(tmp_path):
    script = tmp_path / "mask.py"
    script.write_text(
        "from cortex import on, run\n"
        "@on.request\n"
        "def mask(flow):\n"
        "    print('masking')\n"
        "    flow.replace_text('secret', '***')\n"
        "run(enforce=True)\n"
    )
    msg = anthropic_request("a secret")
    msg["id"] = 7
    proc = subprocess.run(
        [sys.executable, str(script)],
        input=json.dumps(msg) + "\n",
        capture_output=True,
        text=True,
        env={"PYTHONPATH": str(SDK.parent)},
        timeout=30,
    )
    lines = proc.stdout.splitlines()
    assert json.loads(lines[0]) == {"cortex": 1, "hooks": ["request"], "enforce": True, "rewrite_response": False}
    assert lines[1] == "masking"
    reply = json.loads(lines[2])
    assert reply["id"] == 7
    assert "a ***" in reply["request"]["body"]


def test_replay(cortex, monkeypatch):
    @cortex.on.request
    def mask(flow):
        flow.replace_text("secret", "***")

    events = [
        {
            "seq": 1,
            "phase": "request",
            "sessionId": "s",
            "host": "h",
            "inference": {"model": "m", "messages": [{"role": "user", "content": "a secret"}]},
        },
        {"seq": 2, "phase": "response", "inference": {"model": "m"}},
        {
            "seq": 3,
            "phase": "request",
            "sessionId": "s",
            "host": "h",
            "inference": {"model": "m", "messages": [{"role": "user", "content": "nothing"}]},
        },
    ]
    fake_api(cortex, monkeypatch, events)
    out = io.StringIO()
    assert cortex.replay("s", write=lambda s: out.write(s + "\n")) == 0
    text = out.getvalue()
    assert "#1  modify" in text
    assert "replayed 2 requests: 1 modified, 0 denied, 0 errors, 1 unchanged" in text


def test_replay_sees_tool_results(cortex, monkeypatch):
    texts = []

    @cortex.on.request
    def mask(flow):
        texts.append(flow.texts)
        flow.replace_text(r"AKIA[0-9A-Z]{16}", "[key]")

    result = {"toolUseId": "t1", "content": "aws_access_key_id = AKIAIOSFODNN7EXAMPLE"}
    events = [recorded(1, ("user", "read the config"), ("assistant", ""), ("user", ""), tool_results=[result])]
    fake_api(cortex, monkeypatch, events)
    out = io.StringIO()
    cortex.replay("s", write=lambda s: out.write(s + "\n"))
    assert "aws_access_key_id = AKIAIOSFODNN7EXAMPLE" in texts[0]
    assert "#1  modify" in out.getvalue() and "[key]" in out.getvalue()
    assert "replayed 1 requests: 1 modified" in out.getvalue()


def test_replay_reads_latest_text_beside_tool_results_as_live(cortex, monkeypatch):
    seen = []

    @cortex.on.request
    def gate(flow):
        seen.append(flow.latest_text)

    result = {"toolUseId": "t1", "content": "never git push --force"}
    events = [recorded(1, ("user", "please tidy the repo"), ("assistant", ""), ("user", ""), tool_results=[result])]
    fake_api(cortex, monkeypatch, events)
    cortex.replay("s", write=lambda s: None)
    assert seen == ["please tidy the repo"]


def long_session(requests):
    """A session of requests inference requests, each followed by its response."""
    events = []
    for i in range(requests):
        events.append(recorded(2 * i + 1, ("user", "turn %d" % i)))
        events.append({"seq": 2 * i + 2, "phase": "response", "inference": {"model": "m"}})
    return events


def test_replay_covers_a_session_longer_than_one_page(cortex, monkeypatch):
    seen = []

    @cortex.on.request
    def gate(flow):
        seen.append(flow.latest_text)

    fake_api(cortex, monkeypatch, long_session(2500))
    out = io.StringIO()
    assert cortex.replay("s", write=lambda s: out.write(s + "\n")) == 0
    assert seen == ["turn %d" % i for i in range(2500)]  # every request, oldest first
    assert "replayed 2500 requests" in out.getvalue()


def test_replay_reads_one_event_at_a_time(cortex, monkeypatch):
    @cortex.on.request
    def noop(flow):
        pass

    events = long_session(3)
    urls = []

    def get(url):
        urls.append(url)
        return session_api_answer(events, url)

    monkeypatch.setattr(cortex, "_get", get)
    cortex.replay("s", write=lambda s: None)
    # Every page is a summary; message bodies come one event per request.
    pages = [u for u in urls if "/events/" not in u]
    assert pages and all("view=summary" in u for u in pages)
    assert sorted(u.rsplit("/", 1)[1] for u in urls if "/events/" in u) == ["1", "3", "5"]


def test_replay_stops_when_the_session_is_recreated_under_it(cortex, monkeypatch):
    # A cursor above everything held answers with the tail again; replay must not loop.
    @cortex.on.request
    def noop(flow):
        pass

    tail = {"events": [summarized(e) for e in long_session(3)], "totalEvents": 9, "oldestSeq": 1}
    tail["events"] = tail["events"][2:]
    calls = []

    def get(url):
        calls.append(url)
        if "/events/" in url:
            return session_api_answer(long_session(3), url)
        return tail

    monkeypatch.setattr(cortex, "_get", get)
    assert cortex.replay("s", write=lambda s: None) == 1
    assert len([u for u in calls if "/events/" not in u]) <= 2


def test_replay_reports_an_event_it_cannot_read(cortex, monkeypatch):
    @cortex.on.request
    def noop(flow):
        pass

    events = long_session(2)

    def get(url):
        if url.endswith("/events/3"):
            raise OSError("gone")
        return session_api_answer(events, url)

    monkeypatch.setattr(cortex, "_get", get)
    out = io.StringIO()
    assert cortex.replay("s", write=lambda s: out.write(s + "\n")) == 1
    assert "event 3" in out.getvalue()


# ---- OpenAI Responses (Codex) ------------------------------------------------


def responses_request(*items, instructions="You are Codex."):
    body = {"model": "gpt-5-codex", "instructions": instructions, "input": list(items)}
    return {"hook": "request", "session": {"id": "s1"}, "request": {"host": "chatgpt.com", "body": json.dumps(body)}}


def responses_message(role, *texts, part="input_text"):
    return {"type": "message", "role": role, "content": [{"type": part, "text": t} for t in texts]}


CODEX_TURN = (
    responses_message("developer", "be careful with AKIAIOSFODNN7EXAMPLE"),
    responses_message("user", "deploy with AKIAIOSFODNN7EXAMPLE"),
    {"type": "function_call", "call_id": "c1", "name": "shell", "arguments": "{}"},
    {"type": "function_call_output", "call_id": "c1", "output": "key=AKIAIOSFODNN7EXAMPLE"},
)


def test_replace_text_rewrites_a_responses_body(cortex):
    @cortex.on.request
    def mask(flow):
        flow.replace_text(r"AKIA[0-9A-Z]{16}", "[key]")

    reply, _ = cortex.handle(responses_request(*CODEX_TURN, instructions="never print AKIAIOSFODNN7EXAMPLE"))
    body = json.loads(reply["request"]["body"])
    assert body["instructions"] == "never print [key]"
    assert body["input"][0]["content"][0]["text"] == "be careful with [key]"
    assert body["input"][1]["content"][0]["text"] == "deploy with [key]"
    assert body["input"][3]["output"] == "key=[key]"
    assert body["input"][2] == CODEX_TURN[2]


def test_texts_of_a_responses_body(cortex):
    seen = []

    @cortex.on.request
    def look(flow):
        seen.append(flow.texts)

    cortex.handle(responses_request(*CODEX_TURN))
    assert seen == [
        [
            "You are Codex.",
            "be careful with AKIAIOSFODNN7EXAMPLE",
            "deploy with AKIAIOSFODNN7EXAMPLE",
            "key=AKIAIOSFODNN7EXAMPLE",
        ]
    ]


@pytest.mark.parametrize(
    "items, want",
    [
        (CODEX_TURN, "deploy with AKIAIOSFODNN7EXAMPLE"),
        # Without a type, as the API also accepts a message.
        ({"role": "user", "content": "plain"}, "plain"),
        (responses_message("user", "a", "b"), "a\nb"),
        ({"role": "user", "content": [{"type": "input_image", "image_url": "x"}]}, ""),
    ],
)
def test_latest_text_of_a_responses_body(cortex, items, want):
    seen = []

    @cortex.on.request
    def gate(flow):
        seen.append(flow.latest_text)

    items = items if isinstance(items, tuple) else (responses_message("user", "older"), items)
    cortex.handle(responses_request(*items))
    assert seen == [want]


def test_a_responses_input_string_is_the_user_message(cortex):
    seen = []

    @cortex.on.request
    def mask(flow):
        seen.append(flow.latest_text)
        flow.replace_text("secret", "***")

    msg = {"hook": "request", "request": {"body": json.dumps({"model": "gpt-5", "input": "a secret"})}}
    reply, _ = cortex.handle(msg)
    assert seen == ["a secret"]
    assert json.loads(reply["request"]["body"])["input"] == "a ***"


# Two edits compound: the second works on what the first left.
def test_edits_compound(cortex):
    @cortex.on.request
    def two(flow):
        flow.replace_text("AKIA", "[a]")
        flow.replace_text("SECRET", "[s]")

    reply, _ = cortex.handle(anthropic_request("key AKIA and SECRET"))
    assert json.loads(reply["request"]["body"])["messages"][0]["content"][0]["text"] == "key [a] and [s]"


def test_set_body_then_replace_text(cortex):
    @cortex.on.request
    def both(flow):
        body = flow.json
        body["model"] = "smaller"
        flow.set_body(body)
        flow.replace_text("secret", "***")

    reply, _ = cortex.handle(anthropic_request("a secret"))
    out = json.loads(reply["request"]["body"])
    assert out["model"] == "smaller"
    assert out["messages"][0]["content"][0]["text"] == "a ***"


# ---- the documented commands, run as a user would ---------------------------


@pytest.fixture
def session_api():
    """A session API serving one recorded request that carries an AWS key."""
    events = [recorded(1, ("user", "deploy with AKIAIOSFODNN7EXAMPLE"))]

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            try:
                out = json.dumps(session_api_answer(events, self.path)).encode()
            except KeyError:
                self.send_response(404)
                self.end_headers()
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(out)

        def log_message(self, *args):
            pass

    srv = HTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield "http://127.0.0.1:%d" % srv.server_port
    srv.shutdown()


def plugins_folder(home):
    """~/.cortex/plugins under home, with the SDK where Cortex installs it."""
    plugins = home / ".cortex" / "plugins"
    (plugins / "_sdk").mkdir(parents=True)
    shutil.copy(SDK, plugins / "_sdk" / "cortex.py")
    return plugins


def run_in_shell(cmd, home, cwd):
    """cmd in sh, as typed into a terminal: HOME is home and nothing has set PYTHONPATH."""
    env = {k: v for k, v in os.environ.items() if k != "PYTHONPATH"}
    env["HOME"] = str(home)
    return subprocess.run(["sh", "-c", cmd], cwd=cwd, env=env, capture_output=True, text=True, timeout=30)


def test_the_guides_replay_command_runs_from_a_shell(tmp_path, session_api):
    guide = GUIDE.read_text()
    plugins = plugins_folder(tmp_path)
    script = plugins / "10-secrets.py"
    script.write_text(re.findall(r"```python\n(.*?)```", guide, re.S)[0])
    script.chmod(0o755)
    cmd = next(b for b in re.findall(r"```sh\n(.*?)```", guide, re.S) if "--replay" in b).strip()
    proc = run_in_shell(cmd.replace("<session-id>", "s1 " + session_api), tmp_path, tmp_path)
    assert proc.returncode == 0, proc.stderr
    assert "replayed 1 requests: 1 modified" in proc.stdout


def test_the_sdk_docstrings_replay_command_runs_from_a_shell(cortex, tmp_path, session_api):
    plugins = plugins_folder(tmp_path)
    (plugins / "mask.py").write_text(
        "from cortex import on, run\n@on.request\ndef mask(flow):\n    flow.replace_text('AKIA', '[key]')\nrun()\n"
    )
    cmd = next(line.strip() for line in cortex.__doc__.splitlines() if "--replay" in line)
    proc = run_in_shell(cmd.replace("<session-id>", "s1 " + session_api), tmp_path, plugins)
    assert proc.returncode == 0, proc.stderr
    assert "replayed 1 requests: 1 modified" in proc.stdout
