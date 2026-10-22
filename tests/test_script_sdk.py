"""Tests for the script-plugin SDK, core/plugins/script/sdk/cortex.py."""

import importlib.util
import io
import json
import pathlib
import subprocess
import sys

import pytest

SDK = pathlib.Path(__file__).resolve().parent.parent / "core" / "plugins" / "script" / "sdk" / "cortex.py"


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


def test_latest_text_is_only_the_newest_user_message(cortex):
    seen = []

    @cortex.on.request
    def gate(flow):
        seen.append(flow.latest_text)

    cortex.handle(anthropic_request("first rm -rf /", "second"))
    assert seen == ["second"]


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
    monkeypatch.setattr(cortex, "_get", lambda url: {"events": events})
    out = io.StringIO()
    assert cortex.replay("s", write=lambda s: out.write(s + "\n")) == 0
    text = out.getvalue()
    assert "#1  modify" in text
    assert "replayed 2 requests: 1 modified, 0 denied, 0 errors, 1 unchanged" in text


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
