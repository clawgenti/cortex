package inferencerouter

import (
	"strings"
	"testing"
)

func TestLimitRefusal_ReadsEachServersWording(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		kind   limitKind
		n      int
	}{
		{"bedrock output cap", 400, `{"error":{"message":"litellm.BadRequestError: BedrockException - {\"message\":\"The maximum tokens you requested exceeds the model limit of 128000. Try again with a maximum tokens value that is lower than 128000.\"}. Received Model Group=aws/claude-opus-5-5\nAvailable Model Group Fallbacks=None","type":null,"param":null,"code":"400"}}`, limitOutput, 128000},
		{"anthropic output cap", 400, `{"error":{"message":"{\"message\":\"max_tokens: 256000 > 128000, which is the maximum allowed number of output tokens for anthropic.claude-opus-5-5\"}. Received Model Group=aws/claude-opus-5-5\nAvailable Model Group Fallbacks=None","type":"None","param":"None","code":"400"}}`, limitOutput, 128000},
		{"vllm context length", 400, `{"error":{"message":"litellm.ContextWindowExceededError: litellm.BadRequestError: ContextWindowExceededError: Hosted_vllmException - {\"error\":{\"message\":\"This model's maximum context length is 262144 tokens. However, you requested 256000 output tokens and your prompt contains at least 6145 input tokens, for a total of at least 262145 tokens.\"}}","type":null,"param":null,"code":"400"}}`, limitCombined, 262144},
		{"vllm max_model_len", 400, `{"error":{"message":"litellm.BadRequestError: Hosted_vllmException - {\"error\":{\"message\":\"max_completion_tokens=1000000 cannot be greater than max_model_len=max_total_tokens=262144. Please request fewer output tokens.\"}}","type":null,"param":null,"code":"400"}}`, limitCombined, 262144},
		{"vllm max_model_len, plain", 400, `{"error":{"message":"max_completion_tokens=5000000 cannot be greater than max_model_len=1048576. Please request fewer output tokens."}}`, limitCombined, 1048576},
		{"vllm through /v1/messages is a 500", 500, `{"error":{"message":"{\"type\":\"error\",\"error\":{\"type\":\"internal_error\",\"message\":\"max_completion_tokens=1000000 cannot be greater than max_model_len=max_total_tokens=262144.\"}}","type":"None","param":"None","code":"500"}}`, limitCombined, 262144},
		{"anthropic prompt too long", 400, `{"error":{"message":"{\"message\":\"prompt is too long: 260028 tokens > 200000 maximum\"}. Received Model Group=aws/claude-haiku-4-5\nAvailable Model Group Fallbacks=None","type":"None","param":"None","code":"400"}}`, limitWindow, 200000},
		{"bedrock input too long, no number", 400, `{"error":{"message":"litellm.BadRequestError: BedrockException - {\"message\":\"Input is too long for requested model.\"}","type":null,"param":null,"code":"400"}}`, limitWindow, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, n, ok := limitRefusal(tc.status, []byte(tc.body))
			if !ok || kind != tc.kind || n != tc.n {
				t.Errorf("limitRefusal = %v %d %v, want %v %d true", kind, n, ok, tc.kind, tc.n)
			}
		})
	}
}

func TestLimitRefusal_OtherAnswersAreNotLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"model refused", 403, `{"error":{"type":"team_model_access_denied","message":"team not allowed"}}`},
		{"invalid model", 400, `{"error":{"message":"Invalid model name passed in model=exo-free"}}`},
		{"another bad request", 400, `{"error":{"message":"temperature must be at most 2"}}`},
		{"rate limited, wording aside", 429, `{"error":{"message":"maximum context length is 1000 tokens"}}`},
		{"success", 200, `{"choices":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := limitRefusal(tc.status, []byte(tc.body)); ok {
				t.Errorf("limitRefusal recognised %s as a limit refusal", tc.name)
			}
		})
	}
}

func TestLimits_LearnOnlyLowers(t *testing.T) {
	l := limits{}.learn(limitOutput, 128000)
	if l = l.learn(limitOutput, 200000); l.Output != 128000 {
		t.Errorf("Output = %d after a larger refusal, want 128000 kept", l.Output)
	}
	if l = l.learn(limitOutput, 64000); l.Output != 64000 {
		t.Errorf("Output = %d, want 64000", l.Output)
	}
	l = l.learn(limitCombined, 262144)
	if l.Window != 262144 || !l.Combined {
		t.Errorf("limits = %+v, want a combined 262144 window", l)
	}
	if l = l.learn(limitWindow, 300000); l.Window != 262144 {
		t.Errorf("Window = %d after a larger refusal, want 262144 kept", l.Window)
	}
	if got := l.learn(limitWindow, 0); got != l {
		t.Errorf("learn(limitWindow, 0) = %+v, want %+v unchanged", got, l)
	}
	if got := l.learn(limitOutput, 0); got != l {
		t.Errorf("learn(limitOutput, 0) = %+v, want %+v unchanged", got, l)
	}
}

func TestLimits_KeptInTheDurableStoreForThirtyDays(t *testing.T) {
	st := &fakeStore{}
	r, _ := buildRouter(t, wordConfig(`"opencode": "glm"`))
	r.SetStore(st)
	pctx := chat(nil, "glm.example.com", "/v1/chat/completions", opencodeUA, "s1", glmMain, true)

	r.saveLimits(pctx, "glm", glmMain, limits{Window: 262144, Combined: true})
	key := limitPrefix + "glm/" + glmMain
	if st.vals[key] != `{"window":262144,"combined":true}` || st.ttls[key] != limitFor {
		t.Fatalf("stored %q for %v, want the limits for %v", st.vals[key], st.ttls[key], limitFor)
	}
	if got := r.loadLimits(pctx, "glm", glmMain); got != (limits{Window: 262144, Combined: true}) {
		t.Errorf("loadLimits = %+v, want what was saved", got)
	}
	if got := r.loadLimits(pctx, "glm", glmHelper); got != (limits{}) {
		t.Errorf("loadLimits for another model = %+v, want none", got)
	}
}

func TestLimits_WithoutAStoreKeptInProcessMemory(t *testing.T) {
	r, _ := buildRouter(t, wordConfig(`"opencode": "glm"`))
	pctx := chat(newStore(t), "glm.example.com", "/v1/chat/completions", opencodeUA, "s1", glmMain, true)
	r.saveLimits(pctx, "glm", glmMain, limits{Output: 64000})
	if got := r.loadLimits(pctx, "glm", glmMain); got.Output != 64000 {
		t.Errorf("loadLimits = %+v, want Output 64000", got)
	}
}

// An empty error.message does not hide a limit stated in a later field.
func TestLimitRefusal_ReadsPastAnEmptyErrorMessage(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":""},"message":"prompt is too long: 260028 tokens > 200000 maximum"}`,
		`{"error":{"message":""},"message":"","detail":"x"}`,
	} {
		kind, n, ok := limitRefusal(400, []byte(body))
		want := strings.Contains(body, "maximum\"")
		if ok != want || (want && (kind != limitWindow || n != 200000)) {
			t.Errorf("limitRefusal(%s) = %v %d %v, want a window of 200000: %v", body, kind, n, ok, want)
		}
	}
}
