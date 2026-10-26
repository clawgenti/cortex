package sessionapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session/timeline"
)

// timelineMaxEvents bounds how much history one timeline reads. The largest real session seen
// was 5,764 events; this is an order of magnitude past it, so it only binds on a session that
// ran for days, and the response says so.
const timelineMaxEvents = 50_000

// handleTimeline serves the session's spans: agents, tools and hosts on one time axis, for
// agentop's Timeline pane. Computed on every request from the session's whole history, memory
// first and the archive behind it. A 6,000-event session computes in a few milliseconds.
//
// ?from= and ?to= (RFC 3339) keep only the spans that overlap the range; the agent list is
// always whole, so a client can lay out its rows before it has seen every window.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	from, err := timeParam(r, "from")
	if err == nil {
		var to time.Time
		if to, err = timeParam(r, "to"); err == nil {
			events, truncated, ok := s.wholeSession(id)
			if !ok {
				http.NotFound(w, r)
				return
			}
			tl := timeline.Build(id, events)
			tl.Truncated = truncated
			tl.Clip(from, to)
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(tl); err != nil {
				slog.Debug("sessionapi: timeline encode failed", "error", err, "sessionID", id)
			}
			return
		}
	}
	writeUsageError(w, err)
}

// wholeSession returns up to timelineMaxEvents of the session's newest events in Seq order,
// with the archive's history behind what memory holds. truncated reports that older events
// exist past the bound.
func (s *Server) wholeSession(id string) ([]pipeline.SessionEvent, bool, bool) {
	view := s.store.ViewTail(id, timelineMaxEvents)
	if s.archive != nil {
		view = s.withArchivedEvents(id, 0, timelineMaxEvents, view)
	}
	if view == nil {
		return nil, false, false
	}
	return view.Events, view.TotalEvents > len(view.Events), true
}

func timeParam(r *http.Request, name string) (time.Time, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: want an RFC 3339 time, got %q", name, raw)
	}
	return t, nil
}
