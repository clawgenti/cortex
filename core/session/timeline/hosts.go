package timeline

import (
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// buildHosts pairs each outbound request with its response by RequestID. A request with no
// response is left open, which is honest for one in flight and for one whose stream died.
func buildHosts(t *Timeline, events []pipeline.SessionEvent) {
	open := map[string]int{}
	for i := range events {
		e := &events[i]
		if e.Direction != pipeline.Outbound || e.Host == "" {
			continue
		}
		switch e.Phase {
		case pipeline.SessionRequest:
			// A CONNECT the TLS bridge decrypted records an open with no reason, then the
			// decrypted request as its own row. Drawing both doubles every bridged lane.
			if e.Tunnel && e.TunnelReason == "" {
				continue
			}
			open[e.RequestID] = len(t.Hosts)
			t.Hosts = append(t.Hosts, Host{Host: HostKey(e.Host), Kind: kindOf(e), Start: e.At, Seq: e.Seq})
		case pipeline.SessionResponse, pipeline.SessionDenied:
			j, ok := open[e.RequestID]
			if !ok {
				continue
			}
			delete(open, e.RequestID)
			h := &t.Hosts[j]
			// The response's own At can trail the request by minutes more than its duration
			// on a stream the client held open; Duration is what the request took.
			end := e.At
			if e.Duration > 0 {
				end = h.Start.Add(e.Duration)
			}
			h.End, h.Status = &end, e.StatusCode
			h.Error = e.Phase == pipeline.SessionDenied || e.Error != nil || e.StatusCode >= 400
			if h.Kind == KindHTTP && (e.Inference != nil || e.MCP != nil) {
				h.Kind = kindOf(e)
			}
		}
	}
	// A request with no response is in flight only while the session is still near it. Hosts
	// have no agent to move on, so the session's own clock decides.
	for _, j := range open {
		if h := &t.Hosts[j]; t.End.Sub(h.Start) > hostLostAfter {
			h.End, h.Lost = &h.Start, true
		}
	}
}

// hostLostAfter is how long an unanswered request may sit behind the session's newest event
// before it is called lost. Ten minutes covers the longest real model calls (the slowest seen
// was under 22 minutes, and those were the rare tail), while not drawing a dead stream as busy.
const hostLostAfter = 10 * time.Minute

func kindOf(e *pipeline.SessionEvent) Kind {
	switch {
	case e.Inference != nil:
		return KindLLM
	case e.MCP != nil:
		return KindMCP
	case e.Tunnel:
		return KindTunnel
	}
	return KindHTTP
}
