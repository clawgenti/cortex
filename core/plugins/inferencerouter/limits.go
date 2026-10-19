package inferencerouter

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/tidwall/gjson"
)

const (
	// limitPrefix namespaces the limits learned from a server's refusals, by server
	// and model name.
	limitPrefix = Name + "/limit/"

	// limitFor is how long a learned limit is kept. A model's limits rarely change,
	// and a stale one costs only a request trimmed or capped a little early.
	limitFor = 30 * 24 * time.Hour
)

// limitKind is what a limit refusal said was too large.
type limitKind int

const (
	limitOutput   limitKind = iota + 1 // the output field
	limitWindow                        // the input
	limitCombined                      // input and output together, as vLLM counts
)

// limits are what a server's refusals taught about one of its models. Zero means not
// known.
type limits struct {
	Output   int  `json:"output,omitempty"`
	Window   int  `json:"window,omitempty"`
	Combined bool `json:"combined,omitempty"`
}

// limitPatterns are the refusal texts the router reads a limit from, in the wording
// of the servers measured: Bedrock, Anthropic and vLLM, each behind LiteLLM. A pattern
// with an empty group states no number.
var limitPatterns = []struct {
	re   *regexp.Regexp
	kind limitKind
}{
	{regexp.MustCompile(`exceeds the model limit of (\d+)`), limitOutput},
	{regexp.MustCompile(`max_tokens: \d+ > (\d+)`), limitOutput},
	{regexp.MustCompile(`maximum context length is (\d+) tokens`), limitCombined},
	{regexp.MustCompile(`cannot be greater than max_model_len=(?:\w+=)?(\d+)`), limitCombined},
	{regexp.MustCompile(`prompt is too long: \d+ tokens > (\d+) maximum`), limitWindow},
	{regexp.MustCompile(`Input is too long for requested model()`), limitWindow},
}

// limitRefusal reports whether a server's answer refused a request for a limit, which
// kind, and the limit it stated, 0 when it stated none.
func limitRefusal(status int, body []byte) (limitKind, int, bool) {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity, http.StatusInternalServerError:
	default:
		return 0, 0, false
	}
	text := errorMessage(body)
	for _, p := range limitPatterns {
		if m := p.re.FindStringSubmatch(text); m != nil {
			n, _ := strconv.Atoi(m[1])
			return p.kind, n, true
		}
	}
	return 0, 0, false
}

// errorMessage is the message a refusal states — the error's message, a top-level
// message, or the error itself when it is a string — and "" for a body with none.
func errorMessage(body []byte) string {
	for _, path := range []string{"error.message", "message", "error"} {
		if v := gjson.GetBytes(body, path); v.Type == gjson.String && v.String() != "" {
			return v.String()
		}
	}
	return ""
}

const (
	// minOutput and minWindow are the smallest output cap and window learned from a
	// refusal: below them a number names something other than a model's limit.
	minOutput = 256
	minWindow = 1024
)

// plausible reports whether limit n of kind can be the limit that refused a request
// with body: at least the smallest a model has, and, for an output cap, below what
// the request asked for, or, for a window, below the request's estimate and output.
func plausible(kind limitKind, n int, body []byte) bool {
	out := outputValue(body)
	if kind == limitOutput {
		return n >= minOutput && (out == 0 || n < out)
	}
	return n >= minWindow && n < estimate(body)+out
}

// outputValue is the body's output limit, from whichever output field it has, 0 when
// it has none.
func outputValue(body []byte) int {
	for _, f := range outputFields {
		if v := gjson.GetBytes(body, f); v.Type == gjson.Number {
			return int(v.Int())
		}
	}
	return 0
}

// learn is l with a refusal's limit n, which only ever lowers what is known.
func (l limits) learn(kind limitKind, n int) limits {
	if n <= 0 {
		return l // a refusal that states no number teaches no limit
	}
	switch kind {
	case limitOutput:
		if l.Output == 0 || n < l.Output {
			l.Output = n
		}
	case limitWindow, limitCombined:
		if l.Window == 0 || n < l.Window {
			l.Window = n
		}
		if kind == limitCombined {
			l.Combined = true
		}
	}
	return l
}

func limitKey(server, model string) string { return limitPrefix + server + "/" + model }

// loadLimits is what is known about model on server: from the durable store when the
// process has one, else from process memory.
func (p *Router) loadLimits(pctx *pipeline.Context, server, model string) limits {
	key := limitKey(server, model)
	var l limits
	if p.store != nil {
		raw, err := p.store.Get(context.Background(), key)
		if err != nil || raw == "" || json.Unmarshal([]byte(raw), &l) != nil {
			return limits{}
		}
		return l
	}
	if pctx.Shared != nil {
		if v, ok := pctx.Shared.Get(key); ok {
			l, _ = v.(limits)
		}
	}
	return l
}

// saveLimits keeps l for model on server, for limitFor.
func (p *Router) saveLimits(pctx *pipeline.Context, server, model string, l limits) {
	key := limitKey(server, model)
	if p.store != nil {
		raw, _ := json.Marshal(l)
		if err := p.store.Set(context.Background(), key, string(raw), limitFor); err != nil {
			slog.Warn("inference-router: could not keep a learned limit; the next request learns it again", "error", err)
		}
		return
	}
	if pctx.Shared != nil {
		pctx.Shared.Put(key, l, limitFor)
	}
}
