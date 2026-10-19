package inferencerouter

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"

	"github.com/tidwall/gjson"
)

func stripBody(t *testing.T, body string) (string, int) {
	t.Helper()
	out, n := stripReasoning([]byte(body), func(string) bool { return true })
	return string(out), n
}

func TestStrip_AnthropicThinkingBlocks(t *testing.T) {
	out, n := stripBody(t, `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"sig"},
			{"type":"text","text":"hello"},
			{"type":"tool_use","id":"t1","name":"lookup","input":{}}
		]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}
	]}`)
	if n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
	var v struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(v.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(v.Messages))
	}
	if want := `[{"type":"text","text":"hello"},{"type":"tool_use","id":"t1","name":"lookup","input":{}}]`; string(v.Messages[1].Content) != want {
		t.Errorf("assistant content = %s, want %s", v.Messages[1].Content, want)
	}
}

// A thinking-only assistant turn is dropped whole: an empty content array is a
// 400 at Anthropic, and a thinking-only turn has no tool_use for a tool_result
// to reference.
func TestStrip_AThinkingOnlyMessageIsDroppedWhole(t *testing.T) {
	out, n := stripBody(t, `{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"sig"}]},
		{"role":"assistant","content":[{"type":"redacted_thinking","data":"x"}]},
		{"role":"assistant","content":[{"type":"text","text":"hello"}]}
	]}`)
	if n != 2 {
		t.Fatalf("dropped = %d, want 2", n)
	}
	var v struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(v.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(v.Messages))
	}
}

// The user side is never touched: a user's own content, tool results included,
// passes byte-identical.
func TestStrip_LeavesUserMessagesAlone(t *testing.T) {
	body := `{"messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"},{"type":"text","text":"and this"}]}
	]}`
	out, n := stripBody(t, body)
	if n != 0 || string(out) != body {
		t.Errorf("dropped = %d, out = %s; want 0 and unchanged", n, out)
	}
}

func TestStrip_ResponsesReasoningItems(t *testing.T) {
	out, n := stripBody(t, `{"input":[
		{"type":"reasoning","id":"rs_1","encrypted_content":"abc"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"12121"}]},
		{"role":"user","content":[{"type":"input_text","text":"add 1"}]}
	]}`)
	if n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
	var v struct {
		Input []struct {
			Type string `json:"type"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(v.Input) != 2 || v.Input[0].Type != "message" {
		t.Errorf("input = %+v", v.Input)
	}
}

// Chat completions carries reasoning as plain-text fields no measured server
// rejects; the strip must not touch this format at all.
func TestStrip_LeavesChatCompletionsAlone(t *testing.T) {
	body := `{"messages":[
		{"role":"assistant","content":"12121","reasoning_content":"17*23=391","reasoning":"...","thinking_blocks":[{"signature":"sig"}]},
		{"role":"user","content":"add 1"}
	]}`
	out, n := stripBody(t, body)
	if n != 0 || string(out) != body {
		t.Errorf("dropped = %d; want 0 and byte-identical body", n)
	}
}

// A body with nothing strippable is returned unchanged, so the wiring in Task 4
// can skip SetBody entirely.
func TestStrip_NothingToDropReturnsTheBody(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"}],"other":[1,2]}`
	out, n := stripBody(t, body)
	if n != 0 || string(out) != body {
		t.Errorf("dropped = %d; want 0 and unchanged", n)
	}
}

// These tests drive the whole plugin through build/run, like plugin_test.go does.

// thinkingTurn is an assistant turn carrying one thinking block signed sig.
func thinkingTurn(sig string) string {
	return `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"` + sig + `"},{"type":"text","text":"12121"}]}`
}

// history is an Anthropic body: a user turn, then each assistant turn given,
// each followed by a user turn.
func history(turns ...string) []byte {
	msgs := []string{`{"role":"user","content":"Compute 17*23*31."}`}
	for _, t := range turns {
		msgs = append(msgs, t, `{"role":"user","content":"Add 1."}`)
	}
	return []byte(`{"model":"claude-opus-5-5","max_tokens":1024,"messages":[` + strings.Join(msgs, ",") + `]}`)
}

// signatures is every thinking signature pctx's body carries, in order.
func signatures(pctx *pipeline.Context) []string {
	var out []string
	for _, r := range gjson.GetBytes(pctx.Body, `messages.#.content.#(type=="thinking")#.signature`).Array() {
		for _, s := range r.Array() {
			out = append(out, s.String())
		}
	}
	return out
}

// send runs one request of session s1 through config, carrying body.
func send(t *testing.T, store pipeline.SharedStore, config string, body []byte) *pipeline.Context {
	t.Helper()
	pctx := request(store, "api.anthropic.com", claudeUA, "s1")
	pctx.Body = body
	run(t, build(t, config), pctx)
	return pctx
}

func assertSignatures(t *testing.T, pctx *pipeline.Context, want ...string) {
	t.Helper()
	if got := signatures(pctx); !slices.Equal(got, want) {
		t.Errorf("signatures = %v, want %v", got, want)
	}
}

// A session that never moves keeps its reasoning, every turn.
func TestRouter_ASessionThatNeverMovesKeepsItsReasoning(t *testing.T) {
	store := newStore(t)
	glm := routerConfig(`"claude-code": "glm"`)
	send(t, store, glm, history())
	send(t, store, glm, history(thinkingTurn("g1")))
	pctx := send(t, store, glm, history(thinkingTurn("g1"), thinkingTurn("g2")))
	assertSignatures(t, pctx, "g1", "g2")
	assertNoInvocation(t, pctx, "reasoning_stripped")
}

// A move drops, before the first request on the new server is sent, the blocks the
// old server wrote — only that server can read them — and keeps the ones the new
// server writes. Every destination: a server, and the client's own provider,
// whose agent is routed or is moved by the override alone.
func TestRouter_AMoveDropsOnlyTheOldServersReasoning(t *testing.T) {
	for _, tc := range []struct{ name, agents, dest string }{
		{"to a server", `"claude-code": "glm"`, "ete"},
		{"to own, a routed agent", `"claude-code": "glm"`, ""},
		{"to own, an agent the override alone routed", ``, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			before := sessionsConfig(tc.agents, `"s1": "glm"`)
			after := sessionsConfig(tc.agents, `"s1": "`+tc.dest+`"`)
			send(t, store, before, history())
			send(t, store, before, history(thinkingTurn("g1")))

			moved := send(t, store, after, history(thinkingTurn("g1"), thinkingTurn("g2")))
			assertSignatures(t, moved)
			assertAnyRecord(t, moved, pipeline.ActionModify, "reasoning_stripped", map[string]string{"blocks": "2"})
			if got := gjson.GetBytes(moved.Body, "messages.1.content.0.text").String(); got != "12121" {
				t.Errorf("the assistant text was lost: %s", moved.Body)
			}

			next := send(t, store, after, history(thinkingTurn("g1"), thinkingTurn("g2"), thinkingTurn("e1")))
			assertSignatures(t, next, "e1")
		})
	}
}

// A session moved back to a server keeps the blocks that server wrote, and drops
// the ones the server in between wrote.
func TestRouter_AMoveBackKeepsTheServersOwnReasoning(t *testing.T) {
	store := newStore(t)
	onGLM := sessionsConfig(`"claude-code": "glm"`, `"s1": "glm"`)
	onETE := sessionsConfig(`"claude-code": "glm"`, `"s1": "ete"`)
	send(t, store, onGLM, history())
	send(t, store, onGLM, history(thinkingTurn("g1")))
	send(t, store, onETE, history(thinkingTurn("g1"), thinkingTurn("g2")))
	send(t, store, onETE, history(thinkingTurn("g1"), thinkingTurn("g2"), thinkingTurn("e1")))
	back := send(t, store, onGLM, history(thinkingTurn("g1"), thinkingTurn("g2"), thinkingTurn("e1"), thinkingTurn("e2")))
	assertSignatures(t, back, "g1", "g2")
}

// A side request — a title, count_tokens, a subagent — carries none of the
// conversation's blocks, or only its own, and leaves the record of the rest as it
// was: the main conversation's next turn after a move still drops the old blocks.
func TestRouter_ASideRequestLeavesTheRecordAlone(t *testing.T) {
	store := newStore(t)
	before := sessionsConfig(`"claude-code": "glm"`, `"s1": "glm"`)
	after := sessionsConfig(`"claude-code": "glm"`, `"s1": "ete"`)
	send(t, store, before, history())
	send(t, store, before, history(thinkingTurn("g1")))
	send(t, store, after, []byte(`{"model":"claude-haiku-4-5","max_tokens":64,"messages":[{"role":"user","content":"name this"}]}`))
	sub := []byte(`{"model":"claude-opus-5-5","max_tokens":64,"messages":[{"role":"user","content":"Search the repo."}]}`)
	send(t, store, after, sub)
	main := send(t, store, after, history(thinkingTurn("g1"), thinkingTurn("g2")))
	assertSignatures(t, main)
}

// A session first seen carrying reasoning — it ran before routing, or on its own
// provider — has that reasoning taken as the destination's: it is not dropped on
// a guess, and a server that cannot read it answers as it would without the router.
func TestRouter_ReasoningFirstSeenIsTheDestinationsOwn(t *testing.T) {
	pctx := send(t, newStore(t), routerConfig(`"claude-code": "glm"`), history(thinkingTurn("x1")))
	assertSignatures(t, pctx, "x1")
}

// Unsigned thinking blocks with the same text — Ollama writes them, often empty —
// are told apart by where they sit, so a new server's empty block is not taken for
// the old server's.
func TestRouter_EmptyUnsignedBlocksAreToldApart(t *testing.T) {
	store := newStore(t)
	unsigned := `{"role":"assistant","content":[{"type":"thinking","thinking":""},{"type":"text","text":"ok"}]}`
	before := sessionsConfig(`"claude-code": "glm"`, `"s1": "glm"`)
	after := sessionsConfig(`"claude-code": "glm"`, `"s1": "ete"`)
	send(t, store, before, history())
	send(t, store, before, history(unsigned))
	send(t, store, after, history(unsigned))
	next := send(t, store, after, history(unsigned, thinkingTurn("e1"), unsigned))
	if n := strings.Count(string(next.Body), `"type":"thinking"`); n != 2 {
		t.Errorf("thinking blocks = %d, want e1 and the new server's own unsigned block: %s", n, next.Body)
	}
}

// Responses-format reasoning items are recorded and dropped the same way.
func TestRouter_AMoveDropsTheOldServersResponsesReasoning(t *testing.T) {
	store := newStore(t)
	body := func(items ...string) []byte {
		return []byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}` + strings.Join(items, "") + `]}`)
	}
	item := func(id string) string {
		return `,{"type":"reasoning","id":"` + id + `","encrypted_content":"x"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}`
	}
	sendPath := func(config string, b []byte) *pipeline.Context {
		pctx := request(store, "api.anthropic.com", claudeUA, "s1")
		pctx.Path, pctx.Body = "/v1/responses", b
		run(t, build(t, config), pctx)
		return pctx
	}
	onGLM := sessionsConfig(`"claude-code": "glm"`, `"s1": "glm"`)
	sendPath(onGLM, body())
	sendPath(onGLM, body(item("rs_g")))
	onETE := sessionsConfig(`"claude-code": "glm"`, `"s1": "ete"`)
	moved := sendPath(onETE, body(item("rs_g")))
	if ids := gjson.GetBytes(moved.Body, `input.#(type=="reasoning")#.id`).Array(); len(ids) != 0 {
		t.Errorf("moved request's reasoning ids = %v, want none", ids)
	}
	next := sendPath(onETE, body(item("rs_g"), item("rs_e")))
	ids := gjson.GetBytes(next.Body, `input.#(type=="reasoning")#.id`).Array()
	if len(ids) != 1 || ids[0].String() != "rs_e" {
		t.Errorf("reasoning ids = %v, want only rs_e", ids)
	}
}

// Under observe nothing moves, so nothing is dropped or recorded.
func TestRouter_UnderObserveNoReasoningIsDropped(t *testing.T) {
	store := newStore(t)
	send(t, store, routerConfig(`"claude-code": "glm"`), history(thinkingTurn("g1")))
	pctx := request(store, "api.anthropic.com", claudeUA, "s1")
	pctx.Body = history(thinkingTurn("g1"))
	run(t, build(t, sessionsConfig(`"claude-code": "glm"`, `"s1": "ete"`), pipeline.WithPolicies(pipeline.ErrorPolicyObserve)), pctx)
	assertSignatures(t, pctx, "g1")
}

// Synthetic sessions hold many conversations, so nothing is recorded for them.
func TestRouter_NoReasoningRecordForSyntheticSessions(t *testing.T) {
	store := newStore(t)
	pctx := request(store, eteHost, claudeUA, session.DefaultSessionID)
	pctx.Body = history(thinkingTurn("g1"))
	run(t, build(t, routerConfig(`"claude-code": "glm"`)), pctx)
	if _, ok := store.Get(reasoningPrefix + session.DefaultSessionID); ok {
		t.Error("a reasoning record was kept for the default bucket")
	}
}

// assertAnyRecord fails unless SOME invocation of the router is action/reason with
// the details given.
func assertAnyRecord(t *testing.T, pctx *pipeline.Context, action pipeline.InvocationAction, reason string, details map[string]string) {
	t.Helper()
	invs := pctx.Extensions.Invocations
	if invs == nil {
		t.Fatal("the router recorded nothing")
	}
	for _, inv := range invs.Outbound {
		if inv.Plugin != Name || inv.Action != action || inv.Reason != reason {
			continue
		}
		for k, want := range details {
			if got := inv.Details[k]; got != want {
				t.Errorf("Details[%q] = %q, want %q (all: %v)", k, got, want, inv.Details)
			}
		}
		return
	}
	t.Errorf("no %s/%s invocation among %v", action, reason, invs.Outbound)
}

// assertNoInvocation fails when pctx holds an invocation of the router with reason.
func assertNoInvocation(t *testing.T, pctx *pipeline.Context, reason string) {
	t.Helper()
	invs := pctx.Extensions.Invocations
	if invs == nil {
		return
	}
	for _, inv := range invs.Outbound {
		if inv.Plugin == Name && inv.Reason == reason {
			t.Errorf("unexpected %s invocation: %+v", reason, inv)
		}
	}
}

// A session an override moves while it holds no pin — it ran on its own provider,
// its requests skipped at the capture gate — drops its provider's blocks on the
// server it moved to, and gets them back on a move back to its own provider.
func TestRouter_APinlessSessionsMoveDropsItsProvidersReasoning(t *testing.T) {
	store := newStore(t)
	send(t, store, routerConfig(``), history())
	send(t, store, routerConfig(``), history(thinkingTurn("p1")))
	if _, had := pinOf(t, store, "s1"); had {
		t.Fatal("the session is pinned; this case needs none")
	}
	onETE := sessionsConfig(``, `"s1": "ete"`)
	moved := send(t, store, onETE, history(thinkingTurn("p1")))
	assertSignatures(t, moved)
	next := send(t, store, onETE, history(thinkingTurn("p1"), thinkingTurn("e1")))
	assertSignatures(t, next, "e1")
	back := send(t, store, sessionsConfig(``, `"s1": ""`), history(thinkingTurn("p1"), thinkingTurn("e1"), thinkingTurn("e2")))
	assertSignatures(t, back, "p1")
}

// The request that moves a session starts its record whatever it is: one no
// parser read (count_tokens, a model list) uses up the move, so a record it did
// not start would never start. Requests of the session it does not parse follow
// the rule too, a count_tokens carrying the history included.
func TestRouter_AnUnparsedMovingRequestStartsTheRecord(t *testing.T) {
	store := newStore(t)
	onGLM := sessionsConfig(`"claude-code": "glm"`, `"s1": "glm"`)
	onETE := sessionsConfig(`"claude-code": "glm"`, `"s1": "ete"`)
	send(t, store, onGLM, history())
	send(t, store, onGLM, history(thinkingTurn("g1")))

	count := request(store, eteHost, claudeUA, "s1")
	count.Path, count.Body, count.Extensions.Inference = "/v1/messages/count_tokens", history(thinkingTurn("g1")), nil
	run(t, build(t, onETE), count)
	assertSignatures(t, count)

	main := send(t, store, onETE, history(thinkingTurn("g1")))
	assertSignatures(t, main)
}

// A conversation keeps its name across its requests though the client moves its
// cache marker from one message to the next: a block the new server wrote stays
// the new server's on the conversation's second request after the move.
func TestRouter_AConversationsNameSurvivesItsCacheMarker(t *testing.T) {
	store := newStore(t)
	onGLM := sessionsConfig(`"claude-code": "glm"`, `"s1": "glm"`)
	onETE := sessionsConfig(`"claude-code": "glm"`, `"s1": "ete"`)
	send(t, store, onGLM, history())
	send(t, store, onGLM, history(thinkingTurn("g1")))

	marked := `{"role":"user","content":[{"type":"text","text":"A new task.","cache_control":{"type":"ephemeral"}}]}`
	plain := `{"role":"user","content":[{"type":"text","text":"A new task."}]}`
	send(t, store, onETE, []byte(`{"model":"claude-opus-5-5","max_tokens":64,"messages":[`+marked+`]}`))
	second := send(t, store, onETE, []byte(`{"model":"claude-opus-5-5","max_tokens":64,"messages":[`+plain+`,`+thinkingTurn("e1")+`,`+marked+`]}`))
	assertSignatures(t, second, "e1")
}
