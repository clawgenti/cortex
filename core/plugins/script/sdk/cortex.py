"""Write a Cortex plugin as a small Python script.

Put an executable file in ~/.cortex/plugins and Cortex runs it as a step in its
pipeline, one long-lived process per file:

    #!/usr/bin/env python3
    import re
    from cortex import on, run

    AWS = re.compile(r"\\bAKIA[0-9A-Z]{16}\\b")

    @on.request
    def mask(flow):
        flow.replace_text(AWS, "[aws-key]")

    run()                # shadow mode: records what it would do
    # run(enforce=True)  # applies edits and denies

Test it against a session Cortex already recorded before turning it on:

    PYTHONPATH=~/.cortex/plugins/_sdk python3 mask.py --replay <session-id>

Module-level variables keep their values between requests: the process lives as
long as the file is unchanged. print() inside a hook shows on that request's
row in agentop. An exception in a hook is reported on the row and the request
goes through; it does not stop the script.

Runs on Python 3.9 and later, with nothing but the standard library: macOS
starts the Cortex service with the system Python on its PATH.
"""

import base64
import copy
import io
import json
import re
import sys
import traceback
import urllib.request
from contextlib import redirect_stdout

__all__ = ["on", "run", "Flow", "Deny"]

PROTOCOL = 1
SESSION_API = "http://127.0.0.1:47601"


class Deny(Exception):
    """Raised by flow.deny(); ends the hook and blocks the request."""

    def __init__(self, reason, status=403, code="policy.forbidden"):
        super().__init__(reason)
        self.reason, self.status, self.code = reason, status, code


class _Registry:
    """The `on` decorators. Each hook may have several functions; they run in
    the order they were defined."""

    def __init__(self):
        self.hooks = {}
        self.rewrite_response = False

    def request(self, fn):
        """Run fn(flow) on every request, before it is forwarded."""
        self.hooks.setdefault("request", []).append(fn)
        return fn


on = _Registry()


# ---- wire formats -----------------------------------------------------------
#
# A text fragment is a string the user or the agent wrote: a system prompt, a
# message, a tool result. The helpers below find them in Anthropic Messages,
# OpenAI Chat Completions and OpenAI Responses bodies. Anything else is still
# reachable through flow.json.


_TEXT_TYPES = ("text", "input_text", "output_text")


def _walk_content(content, fn):
    """Apply fn to each text in a message's content, returning the new content.
    Content is a string, or a list of blocks: {"type": "text", "text": ...},
    tool results carrying their own content, and others (images) left alone."""
    if isinstance(content, str):
        return fn(content)
    if not isinstance(content, list):
        return content
    out = []
    for block in content:
        if isinstance(block, str):
            out.append(fn(block))
            continue
        if not isinstance(block, dict):
            out.append(block)
            continue
        block = dict(block)
        if block.get("type") in _TEXT_TYPES and isinstance(block.get("text"), str):
            block["text"] = fn(block["text"])
        elif block.get("type") == "tool_result" and "content" in block:
            block["content"] = _walk_content(block["content"], fn)
        out.append(block)
    return out


def _walk_body(body, fn):
    """Apply fn to every prompt text in a request body, in place."""
    if not isinstance(body, dict):
        return
    for key in ("system", "instructions"):
        if key in body:
            body[key] = _walk_content(body[key], fn)
    for msg in body.get("messages") or []:
        if isinstance(msg, dict) and "content" in msg:
            msg["content"] = _walk_content(msg["content"], fn)
    if isinstance(body.get("input"), str):
        body["input"] = fn(body["input"])
    for item in _list(body.get("input")):
        if not isinstance(item, dict):
            continue
        if item.get("type") == "function_call_output" and "output" in item:
            item["output"] = _walk_content(item["output"], fn)
        elif item.get("type", "message") == "message" and "content" in item:
            item["content"] = _walk_content(item["content"], fn)


def _list(v):
    return v if isinstance(v, list) else []


def _is_tool_result(block):
    return isinstance(block, dict) and block.get("type") == "tool_result"


def _messages(body):
    """The conversation, oldest first, as (role, content, tool_output) triples.
    tool_output is content that came back from a tool rather than from a person."""
    out = []
    for msg in _list(body.get("messages")):
        if isinstance(msg, dict):
            content = msg.get("content")
            tool = isinstance(content, list) and all(_is_tool_result(b) for b in content)
            out.append((msg.get("role"), content, tool or msg.get("role") == "tool"))
    if isinstance(body.get("input"), str):
        out.append(("user", body["input"], False))
    for item in _list(body.get("input")):
        if not isinstance(item, dict):
            continue
        if item.get("type") == "function_call_output":
            out.append(("tool", item.get("output"), True))
        elif item.get("type", "message") == "message":
            out.append((item.get("role"), item.get("content"), False))
    return out


def _texts(body):
    found = []
    _walk_body(copy.deepcopy(body), lambda t: found.append(t) or t)
    return found


def _latest_user_text(body):
    """The text of the newest user message that is not only tool results."""
    if not isinstance(body, dict):
        return ""
    for role, content, tool_output in reversed(_messages(body)):
        if role != "user" or tool_output:
            continue
        found = []
        for block in content if isinstance(content, list) else [content]:
            if isinstance(block, dict) and block.get("type") in _TEXT_TYPES:
                block = block.get("text")
            if isinstance(block, str) and block:
                found.append(block)
        return "\n".join(found)
    return ""


def _replacer(pattern, repl):
    if isinstance(pattern, str):
        pattern = re.compile(pattern)
    if callable(pattern) and not hasattr(pattern, "sub"):
        return pattern
    return lambda text: pattern.sub(repl, text)


# ---- one request ------------------------------------------------------------


class Flow:
    """One request (or response) as a hook sees it.

    flow.session      the session id, when Cortex resolved one
    flow.host, .path, .method, .headers
    flow.body         the body as text ("" when it carried none)
    flow.json         the body parsed as JSON, or None; changes to it are sent back
    flow.texts        every prompt text fragment
    flow.latest_text  the newest user message only — what gates should read
    flow.inference    what Cortex's inference parser found (model, tools, ...)
    """

    def __init__(self, msg):
        self._msg = msg
        req = msg.get("request") or {}
        self.hook = msg.get("hook")
        self.session = (msg.get("session") or {}).get("id")
        self.method = req.get("method")
        self.host = req.get("host")
        self.path = req.get("path")
        self.headers = dict(req.get("headers") or {})
        self.inference = req.get("inference")
        self.mcp = req.get("mcp")
        self.a2a = req.get("a2a")
        self.body = _body_of(req)
        self._json = _UNSET
        self._set_headers = {}
        self._remove_headers = []
        self._new_body = None
        self.invocation = None

    def _current(self):
        """The body as edits so far have left it: every helper works on this, so
        two edits compound instead of the second undoing the first."""
        b = self._new_body
        if b is None:
            return self.body
        return b.decode("utf-8", "replace") if isinstance(b, bytes) else b

    @property
    def json(self):
        """The body parsed as JSON, as edits so far have left it, or None."""
        if self._json is _UNSET:
            cur = self._current()
            try:
                self._json = json.loads(cur) if cur else None
            except ValueError:
                self._json = None
        return self._json

    @property
    def texts(self):
        return _texts(self.json)

    @property
    def latest_text(self):
        return _latest_user_text(self.json)

    def replace_text(self, pattern, repl=""):
        """Rewrite every prompt text fragment. pattern is a regex (string or
        compiled) replaced with repl, or a function taking a text and returning
        its replacement. Keep it deterministic: a replacement that differs from
        turn to turn defeats the provider's prompt cache on every request."""
        fn = _replacer(pattern, repl)
        body = self.json
        if body is None:
            return 0
        changed = [0]

        def apply(text):
            new = fn(text)
            if new != text:
                changed[0] += 1
            return new

        _walk_body(body, apply)
        if changed[0]:
            self._new_body = json.dumps(body, ensure_ascii=False, separators=(",", ":"))
        return changed[0]

    def set_body(self, body):
        """Replace the body outright: a str, bytes, or anything JSON-encodable."""
        if body is not None and not isinstance(body, (str, bytes)):
            body = json.dumps(body, ensure_ascii=False, separators=(",", ":"))
        self._new_body = body
        self._json = _UNSET

    def set_header(self, name, value):
        self._set_headers[name] = value

    def remove_header(self, name):
        self._remove_headers.append(name)

    def deny(self, reason, status=403, code="policy.forbidden"):
        raise Deny(reason, status, code)

    def record(self, action="observe", reason="script", **details):
        """Say what this hook did, for its row in agentop. Optional: Cortex
        records modify for an edit, deny for a deny, observe otherwise."""
        self.invocation = {"action": action, "reason": reason, "details": {k: str(v) for k, v in details.items()}}

    def _edits(self):
        e = {}
        if self._new_body is not None:
            if isinstance(self._new_body, bytes):
                e["body_base64"] = base64.b64encode(self._new_body).decode()
            else:
                e["body"] = self._new_body
        if self._set_headers:
            e["set_headers"] = self._set_headers
        if self._remove_headers:
            e["remove_headers"] = self._remove_headers
        return e


_UNSET = object()


def _body_of(part):
    if "body_base64" in part:
        try:
            return base64.b64decode(part["body_base64"]).decode("utf-8", "replace")
        except ValueError:
            return ""
    return part.get("body") or ""


def handle(msg):
    """Run the hooks for one message and return the reply, without the id."""
    hook = msg.get("hook")
    flow = Flow(msg)
    out = io.StringIO()
    reply = {}
    try:
        with redirect_stdout(out):
            for fn in on.hooks.get(hook, []):
                fn(flow)
        edits = flow._edits()
        if edits:
            reply[hook] = edits
        if flow.invocation:
            reply["invocation"] = flow.invocation
    except Deny as d:
        reply = {"deny": {"reason": d.reason, "status": d.status, "code": d.code}}
    except Exception:  # a bug in the hook must not end the script
        reply = {"error": traceback.format_exc(limit=3).strip()}
    try:
        _encode(reply)
    except Exception as e:
        reply = {"error": "".join(traceback.format_exception_only(type(e), e)).strip()}
    printed = out.getvalue().rstrip("\n")
    return reply, printed


# ---- the worker loop --------------------------------------------------------


def run(enforce=False):
    """Serve Cortex, or with --replay <session-id>, test against a recorded
    session. Call it last, after every hook is defined."""
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(errors="backslashreplace")
    if len(sys.argv) >= 3 and sys.argv[1] == "--replay":
        sys.exit(replay(sys.argv[2], api=(sys.argv[3] if len(sys.argv) > 3 else SESSION_API)))
    out = sys.stdout
    _send(
        out,
        {
            "cortex": PROTOCOL,
            "hooks": sorted(on.hooks),
            "enforce": bool(enforce),
            "rewrite_response": on.rewrite_response,
        },
    )
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except ValueError:
            continue
        reply, printed = handle(msg)
        for p in printed.splitlines():
            out.write(p + "\n")  # printed output goes before the reply, as plain lines
        reply["id"] = msg.get("id")
        _send(out, reply)


def _encode(obj):
    return json.dumps(obj, separators=(",", ":"), allow_nan=False)


def _send(out, obj):
    out.write(_encode(obj) + "\n")
    out.flush()


# ---- replay -----------------------------------------------------------------


def _get(url):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))  # never through Cortex
    with opener.open(url, timeout=30) as r:
        return json.load(r)


def _rebuild_request(event):
    """A recorded inference request, rebuilt as an Anthropic-shaped body from the
    messages the parser recorded. Recorded events do not carry raw bodies, so a
    hook that reads only prompt text sees what it would have live."""
    inf = event.get("inference") or {}
    system, messages = [], []
    for m in inf.get("messages") or []:
        block = [{"type": "text", "text": m["content"]}] if m.get("content") else []
        if m.get("role") == "system":
            system += block
        else:
            messages.append({"role": m.get("role"), "content": block})
    results = [
        {"type": "tool_result", "tool_use_id": r.get("toolUseId", ""), "content": r.get("content") or ""}
        for r in inf.get("toolResults") or []
    ]
    if results:
        messages.append({"role": "user", "content": results})
    body = {"model": inf.get("model"), "messages": messages}
    if system:
        body["system"] = system
    return {
        "hook": "request",
        "session": {"id": event.get("sessionId")},
        "request": {
            "method": event.get("httpMethod"),
            "host": event.get("host"),
            "path": event.get("httpPath"),
            "headers": {},
            "body": json.dumps(body, ensure_ascii=False, separators=(",", ":")),
            "inference": inf,
        },
    }


def replay(session_id, api=SESSION_API, write=print):
    """Run the request hooks over every inference request a session recorded and
    report what they would have done. Returns a process exit code."""
    if "request" not in on.hooks:
        write("no @on.request hook to replay")
        return 1
    try:
        seqs = _request_seqs(session_id, api)
    except Exception as e:
        write("cannot read session %s from %s: %s" % (session_id, api, e))
        return 1
    rows = {"modify": 0, "deny": 0, "error": 0, "unchanged": 0}
    for seq in seqs:
        try:
            ev = _get("%s/v1/sessions/%s/events/%d" % (api, session_id, seq))
        except Exception as e:
            write("cannot read event %d of session %s from %s: %s" % (seq, session_id, api, e))
            return 1
        msg = _rebuild_request(ev)
        before = _texts(json.loads(msg["request"]["body"]))
        reply, printed = handle(msg)
        seq = ev.get("seq")
        after = before
        if "body" in (reply.get("request") or {}):
            try:
                after = _texts(json.loads(reply["request"]["body"]))
            except ValueError:
                after = None
        if "deny" in reply:
            rows["deny"] += 1
            write("#%s  deny    %s" % (seq, reply["deny"]["reason"]))
        elif "error" in reply:
            rows["error"] += 1
            write("#%s  error   %s" % (seq, reply["error"].splitlines()[-1]))
        elif after != before:
            rows["modify"] += 1
            write("#%s  modify  %s" % (seq, _diff_summary(before, after)))
        else:
            rows["unchanged"] += 1
        for p in printed.splitlines():
            write("#%s  print   %s" % (seq, p))
    total = sum(rows.values())
    write(
        "replayed %d requests: %d modified, %d denied, %d errors, %d unchanged"
        % (total, rows["modify"], rows["deny"], rows["error"], rows["unchanged"])
    )
    return 0


def _request_seqs(session_id, api):
    """The seq of each inference request the session recorded, oldest first. Pages
    back from the newest event in summaries, which leave out message bodies."""
    seqs, before = [], 0
    while True:
        url = "%s/v1/sessions/%s?view=summary&limit=2000" % (api, session_id)
        view = _get(url + ("&before=%d" % before if before else ""))
        events = view.get("events") or []
        for ev in events:
            inf = ev.get("inference") or {}
            if ev.get("phase") == "request" and (inf.get("messageCount") or inf.get("messages")):
                seqs.append(ev["seq"])
        if not events or not view.get("totalEvents") or events[0]["seq"] <= view.get("oldestSeq", 0):
            return sorted(set(seqs))
        if before and events[0]["seq"] >= before:
            raise RuntimeError("the session changed while it was read; run the replay again")
        before = events[0]["seq"]


def _diff_summary(before, after):
    """The first text fragment that changed, around where it changed. Judged on
    the text a hook rewrote rather than on body bytes, which a re-encoding alone
    would change."""
    if after is None:
        return "body is no longer JSON"
    for b, a in zip(before, after):
        if b != a:
            i = 0
            while i < min(len(b), len(a)) and b[i] == a[i]:
                i += 1
            lo = max(0, i - 20)
            return "%r -> %r" % (b[lo : i + 30], a[lo : i + 30])
    return "%d text fragments -> %d" % (len(before), len(after))
