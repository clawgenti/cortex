package sessionapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session/timeline"
)

// timelineMaxEvents bounds how much history one timeline reads. The largest real session seen
// was 5,764 events; this is an order of magnitude past it, so it only binds on a session that
// ran for days, and the response says so.
const timelineMaxEvents = 50_000

// handleTimeline serves the session's spans: agents, tools and hosts on one time axis, for
// agentop's Timeline pane. Computed from the session's whole history, memory first and the
// archive behind it. A 6,000-event session computes in a few milliseconds; reading one that
// lives only on disk takes over a second, so the result is cached per session until the
// session's newest event changes, and requests for one session share one compute.
//
// ?from= and ?to= (RFC 3339) keep only the spans that overlap the range; the agent list is
// always whole, so a client can lay out its rows before it has seen every window.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	from, err := timeParam(r, "from")
	if err == nil {
		var to time.Time
		if to, err = timeParam(r, "to"); err == nil {
			tl, ok := s.timelines.get(s, id)
			if !ok {
				http.NotFound(w, r)
				return
			}
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

// timelineCache holds the last computed timeline per session, keyed by the newest Seq the
// store held when it was built. Bounded: a laptop proxy holds at most a few hundred sessions,
// and only those someone opened the pane on are here.
type timelineCache struct {
	mu      sync.Mutex
	entries map[string]timelineEntry
	flight  singleflight.Group
}

type timelineEntry struct {
	seq uint64
	at  time.Time
	tl  *timeline.Timeline
}

// timelineCacheMax bounds the cache; past it the oldest entry goes.
const timelineCacheMax = 32

// timelineStale is how long a cached timeline of an idle session is trusted: Build ages
// unanswered requests against now, so the same events can draw differently later.
const timelineStale = time.Minute

// get returns a private copy, since the caller clips it.
func (c *timelineCache) get(s *Server, id string) (*timeline.Timeline, bool) {
	// The session's newest Seq: memory's when it holds the session, else the archive's,
	// which for a session only on disk never moves.
	newest := uint64(0)
	if v := s.store.ViewTail(id, 1); v != nil && len(v.Events) > 0 {
		newest = v.Events[0].Seq
	} else if s.archive != nil {
		newest = s.archive.LastSeq(id)
	}
	now := s.clock()
	c.mu.Lock()
	e, hit := c.entries[id]
	c.mu.Unlock()
	if hit && newest != 0 && e.seq == newest && now.Sub(e.at) < timelineStale {
		return clone(e.tl), true
	}
	v, _, _ := c.flight.Do(id, func() (any, error) {
		events, truncated, ok := s.wholeSession(id)
		if !ok {
			return nil, nil
		}
		tl := timeline.Build(id, events, now)
		tl.Truncated = truncated
		if newest != 0 {
			c.put(id, timelineEntry{seq: newest, at: now, tl: tl})
		}
		return tl, nil
	})
	tl, _ := v.(*timeline.Timeline)
	if tl == nil {
		return nil, false
	}
	return clone(tl), true
}

func (c *timelineCache) put(id string, e timelineEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]timelineEntry{}
	}
	if _, ok := c.entries[id]; !ok && len(c.entries) >= timelineCacheMax {
		var oldest string
		for k, v := range c.entries {
			if oldest == "" || v.at.Before(c.entries[oldest].at) {
				oldest = k
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[id] = e
}

// clone copies the span slices Clip rewrites. The spans' time pointers are shared, which
// is safe: Clip never writes through them.
func clone(tl *timeline.Timeline) *timeline.Timeline {
	cp := *tl
	cp.Agents = append([]timeline.Agent(nil), tl.Agents...)
	cp.Turns = append([]timeline.Turn(nil), tl.Turns...)
	cp.Tools = append([]timeline.Tool(nil), tl.Tools...)
	cp.Returns = append([]timeline.Return(nil), tl.Returns...)
	cp.Hosts = append([]timeline.Host(nil), tl.Hosts...)
	return &cp
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
	// Truncated when the oldest event served is not the oldest held. Both the store and the
	// archive merge set OldestSeq only when events were left out. TotalEvents is not the
	// signal: the merge reports the larger of two counts, which can equal what was served.
	truncated := view.OldestSeq != 0 && len(view.Events) > 0 && view.Events[0].Seq > view.OldestSeq
	return view.Events, truncated, true
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
