package inferencerouter

import (
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var pad = strings.Repeat("w", 500)

func user(text string) string { return `{"role":"user","content":` + strconv.Quote(text) + `}` }
func asst(text string) string { return `{"role":"assistant","content":` + strconv.Quote(text) + `}` }
func toolResult(text string) string {
	return `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":` + strconv.Quote(text) + `}]}`
}

// anthropicBody is a /v1/messages body with msgs.
func anthropicBody(msgs ...string) []byte {
	return []byte(`{"model":"m","max_tokens":100,"system":"be helpful","tools":[{"name":"Read"}],"messages":[` + strings.Join(msgs, ",") + `]}`)
}

// openaiBody is a chat-completions body with a leading system message and msgs.
func openaiBody(msgs ...string) []byte {
	return []byte(`{"model":"m","max_completion_tokens":100,"messages":[{"role":"system","content":"be helpful"},` + strings.Join(msgs, ",") + `]}`)
}

func TestFitBody_LowersOnlyTheOutputFieldAndNeverRaisesIt(t *testing.T) {
	for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		t.Run(field, func(t *testing.T) {
			body := []byte(`{"model":"m","` + field + `":256000,"messages":[{"role":"user","content":"hi"}]}`)
			r := fitBody(body, limits{Output: 128000}, 0)
			want, _ := sjson.SetBytes(body, field, 128000)
			if string(r.body) != string(want) {
				t.Fatalf("body = %s, want only %s changed: %s", r.body, field, want)
			}
			if r.field != field || r.from != 256000 || r.to != 128000 {
				t.Errorf("result = %s %d→%d, want %s 256000→128000", r.field, r.from, r.to, field)
			}
			if r := fitBody(body, limits{Output: 300000}, 0); string(r.body) != string(body) || r.field != "" {
				t.Errorf("a cap above the request's value changed it: %s", r.body)
			}
		})
	}
}

func TestFitBody_LeavesAMissingOrNullOutputFieldAlone(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","max_tokens":null,"messages":[{"role":"user","content":"hi"}]}`,
	} {
		if r := fitBody([]byte(body), limits{Output: 1000, Window: 100000, Combined: true}, 0); string(r.body) != body {
			t.Errorf("body %s became %s, want it unchanged", body, r.body)
		}
	}
}

func TestFitBody_ACombinedWindowLeavesRoomForThePrompt(t *testing.T) {
	body := []byte(`{"model":"m","max_completion_tokens":256000,"messages":[{"role":"user","content":"hi"}]}`)
	r := fitBody(body, limits{Window: 256000, Combined: true}, 0)
	if want := 256000 - estimate(r.body); r.to != want {
		t.Errorf("max_completion_tokens = %d, want window − estimate = %d", r.to, want)
	}
	// The estimate runs high, so a prompt it puts at or over the window may still
	// fit: the output field is left as the client sent it.
	huge := []byte(`{"model":"m","max_tokens":5000,"messages":[{"role":"user","content":"` + strings.Repeat("w", 9000) + `"}]}`)
	if r := fitBody(huge, limits{Window: 1000, Combined: true}, 0); r.field != "" || gjson.GetBytes(r.body, "max_tokens").Int() != 5000 {
		t.Errorf("result = %s %d, body %s; want max_tokens left at 5000", r.field, r.to, r.body)
	}
	// A plain output cap still applies there.
	if r := fitBody(huge, limits{Output: 2000, Window: 1000, Combined: true}, 0); r.field != "max_tokens" || r.to != 2000 {
		t.Errorf("result = %s %d, want max_tokens capped at the output limit 2000", r.field, r.to)
	}
}

func TestFitBody_TrimsTheOldestTurnsOfAnAnthropicConversation(t *testing.T) {
	body := anthropicBody(user("the task"), asst(pad), user(pad), asst(pad), toolResult(pad), asst(pad), user(pad), asst(pad), user("latest"))
	r := fitBody(body, limits{Window: 1000}, 0)
	if r.dropped == 0 || r.unfittable {
		t.Fatalf("result = %+v, want turns dropped", r)
	}
	if r.estimate > 600 {
		t.Errorf("estimate = %d after trimming, want at most 60%% of 1000", r.estimate)
	}
	msgs := gjson.GetBytes(r.body, "messages").Array()
	if !strings.HasPrefix(msgs[0].Get("content").String(), "the task") {
		t.Errorf("first message = %s, want the task kept", msgs[0].Raw)
	}
	first := msgs[1]
	if first.Get("role").String() != "user" || first.Get("content").IsArray() {
		t.Errorf("kept tail starts with %s, want a plain user turn", first.Raw)
	}
	wantNote := "[Cortex: " + strconv.Itoa(r.dropped) + " earlier messages were removed so the conversation fits this model's context window.]"
	if !strings.HasPrefix(first.Get("content").String(), wantNote) {
		t.Errorf("first kept message = %q, want it to start with the note", first.Get("content").String())
	}
	if msgs[len(msgs)-1].Get("content").String() != "latest" {
		t.Error("the latest turn was dropped")
	}
	if gjson.GetBytes(r.body, "system").String() != "be helpful" || gjson.GetBytes(r.body, "tools.0.name").String() != "Read" {
		t.Error("system or tools changed")
	}
}

func TestFitBody_TrimsAnOpenAIConversationWithoutSplittingAToolCall(t *testing.T) {
	call := `{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]}`
	tool := `{"role":"tool","tool_call_id":"c1","content":` + strconv.Quote(pad) + `}`
	body := openaiBody(user("the task"), call, tool, asst(pad), user(pad), call, tool, asst(pad), user(pad), asst(pad), user("latest"))
	r := fitBody(body, limits{Window: 1000}, 0)
	if r.dropped == 0 || r.estimate > 600 {
		t.Fatalf("result = %+v, want turns dropped to at most 600", r)
	}
	msgs := gjson.GetBytes(r.body, "messages").Array()
	if msgs[0].Get("role").String() != "system" || !strings.HasPrefix(msgs[1].Get("content").String(), "the task") {
		t.Errorf("head = %s %s, want the system message and the task", msgs[0].Raw, msgs[1].Raw)
	}
	if role := msgs[2].Get("role").String(); role != "user" {
		t.Errorf("kept tail starts with role %q, want user: a tool result must not lose its call", role)
	}
}

func TestFitBody_KeepsTheCutWhileTheConversationStillFits(t *testing.T) {
	turns := []string{user("the task"), asst(pad), user(pad), asst(pad), user(pad), asst(pad), user(pad), asst(pad), user("latest")}
	l := limits{Window: 1000}
	first := fitBody(anthropicBody(turns...), l, 0)
	if first.cut == 0 {
		t.Fatal("the first fit dropped nothing")
	}
	// One more short exchange: the same cut, so the request's start is unchanged.
	grown := append(append([]string{}, turns...), asst("ok"), user("next"))
	if again := fitBody(anthropicBody(grown...), l, first.cut); again.cut != first.cut {
		t.Errorf("cut = %d after a short exchange, want %d kept", again.cut, first.cut)
	}
	// Many more long turns: past 85% with the old cut, so a new, later cut.
	long := append([]string{}, grown...)
	for i := 0; i < 4; i++ {
		long = append(long, asst(pad), user(pad))
	}
	if moved := fitBody(anthropicBody(long...), l, first.cut); moved.cut <= first.cut {
		t.Errorf("cut = %d after the conversation grew past 85%%, want later than %d", moved.cut, first.cut)
	}
}

func TestFitBody_LeavesWhatCannotBeTrimmedUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		cut  int
	}{
		{"no user message", []byte(`{"model":"m","messages":[{"role":"assistant","content":"` + strings.Repeat("w", 9000) + `"}]}`), 0},
		{"a stale cut past the end", anthropicBody(user("the task"), asst("ok"), user("short")), 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if r := fitBody(tc.body, limits{Window: 1000}, tc.cut); string(r.body) != string(tc.body) || r.dropped != 0 {
				t.Errorf("body changed: %s", r.body)
			}
		})
	}
	// One turn larger than the window: nothing to drop that would help.
	huge := anthropicBody(user("the task"), asst("ok"), user(strings.Repeat("w", 9000)))
	if r := fitBody(huge, limits{Window: 1000}, 0); !r.unfittable || string(r.body) != string(huge) {
		t.Errorf("result = %+v, want unfittable and the body unchanged", r)
	}
}

func TestCutPoint_AToolResultIsNot(t *testing.T) {
	mixed := gjson.Parse(`{"role":"user","content":[{"type":"text","text":"see"},{"type":"tool_result","tool_use_id":"t1","content":"x"}]}`)
	if cutPoint(mixed) || cutPoint(gjson.Parse(`{"role":"tool","tool_call_id":"c1","content":"x"}`)) {
		t.Error("a tool result was taken as a cut point")
	}
	if !cutPoint(gjson.Parse(user("hi"))) || cutPoint(gjson.Parse(asst("hi"))) {
		t.Error("cutPoint misjudged a plain user or assistant message")
	}
}

// Anthropic and Bedrock refuse a max_tokens that is not above thinking.budget_tokens,
// so a cap that would reach the budget leaves the field alone.
func TestFitBody_NeverCapsOutputToOrBelowTheThinkingBudget(t *testing.T) {
	body := func(budget int) []byte {
		return []byte(`{"model":"m","max_tokens":128000,"thinking":{"type":"enabled","budget_tokens":` + strconv.Itoa(budget) + `},"messages":[{"role":"user","content":"hi"}]}`)
	}
	if r := fitBody(body(100000), limits{Output: 64000}, 0); string(r.body) != string(body(100000)) || r.field != "" {
		t.Errorf("budget 100000: body = %s, want it unchanged", r.body)
	}
	r := fitBody(body(10000), limits{Output: 64000}, 0)
	if got := gjson.GetBytes(r.body, "max_tokens").Int(); got != 64000 {
		t.Errorf("budget 10000: max_tokens = %d, want 64000", got)
	}
}

// A stored cut applies only while the request still needs trimming: one that fits is
// sent whole, whatever cut the session holds.
func TestFitBody_ACutIsNotReusedOnARequestThatFits(t *testing.T) {
	small := anthropicBody(user("the task"), asst("a"), user("b"), asst("c"), user("d"), asst("e"), user("latest"))
	for _, cut := range []int{2, 4, 6} {
		if r := fitBody(small, limits{Window: 100000}, cut); string(r.body) != string(small) || r.dropped != 0 {
			t.Errorf("cut %d: %d messages dropped from a request far inside the window", cut, r.dropped)
		}
	}
}

func toolUse(id string) string {
	return `{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"Read","input":{}}]}`
}
func toolOutput(id, text string) string {
	return `{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":` + strconv.Quote(text) + `}]}`
}

// One prompt followed by many tool calls has no user message to cut at: it is
// unfittable and sent unchanged, never cut inside the task.
func TestFitBody_DoesNotCutInsideOneLongToolCallingTurn(t *testing.T) {
	anthropic := []string{user("the task")}
	openai := []string{user("the task")}
	for i := 0; i < 30; i++ {
		id := "t" + strconv.Itoa(i)
		anthropic = append(anthropic, toolUse(id), toolOutput(id, pad))
		openai = append(openai,
			`{"role":"assistant","content":null,"tool_calls":[{"id":"`+id+`","type":"function","function":{"name":"Read","arguments":"{}"}}]}`,
			`{"role":"tool","tool_call_id":"`+id+`","content":`+strconv.Quote(pad)+`}`)
	}
	for name, body := range map[string][]byte{"anthropic": anthropicBody(anthropic...), "openai": openaiBody(openai...)} {
		t.Run(name, func(t *testing.T) {
			r := fitBody(body, limits{Window: 4000}, 0)
			if r.dropped != 0 || !r.unfittable {
				t.Errorf("result = dropped %d unfittable %v, want nothing dropped and unfittable", r.dropped, r.unfittable)
			}
		})
	}
}
