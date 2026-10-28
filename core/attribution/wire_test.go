package attribution

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// What Encode writes Decode reads back exactly, for every shape Resolve returns.
func TestWire_RoundTrips(t *testing.T) {
	for _, a := range []Attribution{
		{Agent: "claude-code", Proc: "100.5", Session: "s1", Header: session.ClaudeCodeSessionHeader,
			Source: pipeline.SessionSourceHeader, ChainSession: "outer"},
		{Agent: "bob", Proc: "820.1", Source: pipeline.SessionSourcePending},
		{Proc: "700.9", Session: session.DefaultSessionID},
		{Agent: "claude-code", Session: "s2", Source: pipeline.SessionSourceAffinity},
	} {
		h := http.Header{}
		Encode(h, a)
		got, ok := Decode(h)
		if !ok || got != a {
			t.Errorf("Decode(Encode(%+v)) = %+v, %v", a, got, ok)
		}
	}
}

// A request that carries none decodes to nothing; a malformed one is refused rather than
// half-read, so the recorder resolves the request itself.
func TestWire_DecodeRefusesWhatItCannotTrust(t *testing.T) {
	if _, ok := Decode(http.Header{}); ok {
		t.Error("an empty header decoded")
	}
	for name, h := range map[string]http.Header{
		"unknown source":      {HeaderSource: {"guess"}, HeaderSession: {"s1"}},
		"control in session":  {HeaderSession: {"s1\n"}},
		"oversized session":   {HeaderSession: {strings.Repeat("x", session.MaxSessionIDLen+1)}},
		"process not a key":   {HeaderProc: {"claude"}},
		"process half a key":  {HeaderProc: {"100."}},
		"control in agent":    {HeaderAgent: {"cl\x1baude"}, HeaderProc: {"1.2"}},
		"control in ancestor": {HeaderSession: {"s1"}, HeaderChain: {"o\x7f"}},
	} {
		if a, ok := Decode(h); ok {
			t.Errorf("%s: decoded %+v", name, a)
		}
	}
}

// Strip removes every attribution header and nothing else.
func TestWire_StripRemovesOnlyAttribution(t *testing.T) {
	h := http.Header{}
	Encode(h, Attribution{Agent: "a", Proc: "1.2", Session: "s", Source: pipeline.SessionSourceHeader, Header: "X", ChainSession: "c"})
	h.Set("User-Agent", "curl/8")
	Strip(h)
	if len(h) != 1 || h.Get("User-Agent") != "curl/8" {
		t.Errorf("after Strip: %v, want only User-Agent", h)
	}
}
