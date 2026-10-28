package attribution

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// The wire form of an Attribution: what a hop that resolved a request — a local relay that
// ran Resolve at accept time — stamps on it for the recorder behind it, which then trusts the
// stamp instead of resolving again. That is how attribution can run outside the proxy that
// records: the relay owns the process lookup, the recorder owns the events.
//
// STRIP, THEN INJECT. A client can send these headers too, and one that did would choose its
// own session and process. So every listener removes them from what the client sent (Strip),
// and only a hop the deployment configures as trusted may set them; Decode is for that hop's
// requests alone. No such hop is configured today, so nothing decodes: the contract exists so
// the relay and the recorder can be built against it separately.
const (
	HeaderAgent   = "X-Cortex-Agent"
	HeaderProc    = "X-Cortex-Proc"
	HeaderSession = "X-Cortex-Session"
	HeaderSource  = "X-Cortex-Source"
	// HeaderNamedBy is the session-id header that named HeaderSession, when one did.
	HeaderNamedBy = "X-Cortex-Session-Header"
	// HeaderChain is ChainSession.
	HeaderChain = "X-Cortex-Chain-Session"
)

var wireHeaders = []string{HeaderAgent, HeaderProc, HeaderSession, HeaderSource, HeaderNamedBy, HeaderChain}

// Strip removes every attribution header from h, so a client cannot assert one.
func Strip(h http.Header) {
	for _, k := range wireHeaders {
		h.Del(k)
	}
}

// Encode sets a's headers on h, replacing any there. An empty field sets no header.
func Encode(h http.Header, a Attribution) {
	Strip(h)
	for k, v := range map[string]string{
		HeaderAgent: a.Agent, HeaderProc: a.Proc, HeaderSession: a.Session,
		HeaderSource: string(a.Source), HeaderNamedBy: a.Header, HeaderChain: a.ChainSession,
	} {
		if v != "" {
			h.Set(k, v)
		}
	}
}

// Decode reads an Attribution a trusted hop set on h. ok is false when h carries none, or
// carries one that is malformed — an unknown source, a session id the store would refuse, a
// process that is not "<pid>.<start>" — and then the caller resolves the request itself.
func Decode(h http.Header) (Attribution, bool) {
	if h.Get(HeaderSource) == "" && h.Get(HeaderSession) == "" && h.Get(HeaderProc) == "" {
		return Attribution{}, false
	}
	a := Attribution{
		Agent:        h.Get(HeaderAgent),
		Proc:         h.Get(HeaderProc),
		Session:      h.Get(HeaderSession),
		Source:       pipeline.SessionSource(h.Get(HeaderSource)),
		Header:       h.Get(HeaderNamedBy),
		ChainSession: h.Get(HeaderChain),
	}
	switch a.Source {
	case "", pipeline.SessionSourceHeader, pipeline.SessionSourceProcess, pipeline.SessionSourcePending,
		pipeline.SessionSourceAffinity, pipeline.SessionSourceActive:
	default:
		return Attribution{}, false
	}
	for _, id := range []string{a.Session, a.ChainSession} {
		if id != "" && (len(id) > session.MaxSessionIDLen || strings.ContainsFunc(id, isControl)) {
			return Attribution{}, false
		}
	}
	if a.Proc != "" && !validProc(a.Proc) {
		return Attribution{}, false
	}
	if a.Agent != "" && strings.ContainsFunc(a.Agent, isControl) {
		return Attribution{}, false
	}
	return a, true
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// validProc reports whether p is Proc.Key's form: a pid, a dot, a start time.
func validProc(p string) bool {
	pid, start, ok := strings.Cut(p, ".")
	if !ok {
		return false
	}
	if _, err := strconv.ParseInt(pid, 10, 32); err != nil {
		return false
	}
	_, err := strconv.ParseInt(start, 10, 64)
	return err == nil
}
