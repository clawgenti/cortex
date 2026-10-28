package tui

import (
	"context"
	"slices"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// A process's calls before it named a session are recorded in its pending bucket and linked
// to the session at read time (session.Link). agentop folds them into that session: a fully
// linked bucket's row is hidden from the list, and opening the session merges the calls into
// its timeline at the time they happened, each event still saying it was its process's
// (SessionID is the bucket's). A partly linked bucket stays a row of its own and also feeds
// the sessions it links to.

// linkedInto is, for each session, the bucket windows that feed it: which bucket, and the
// window of that bucket's requests it takes (session.LinkedEvents).
type linkedWindow struct {
	bucket        string
	after, before time.Time
}

func (m *model) linkedInto(id string) []linkedWindow {
	var out []linkedWindow
	for _, s := range m.sessions {
		var after time.Time
		for _, l := range s.LinkedTo {
			if l.Session == id {
				out = append(out, linkedWindow{bucket: s.ID, after: after, before: l.Until})
			}
			after = l.Until
		}
	}
	return out
}

// hiddenAsLinked reports whether the list hides id: a pending bucket every request of which
// belongs to a session it names, so its calls are read inside those sessions instead.
func (m *model) hiddenAsLinked(id string) bool {
	for _, s := range m.sessions {
		if s.ID == id {
			return s.LinkedAll
		}
	}
	return false
}

// linkedEventsCap bounds what one session's linked buckets add to its first snapshot. A bucket
// is a process's start-up calls, a dozen events; this only stops a chatty process (an SDK
// probe loop) from doubling a snapshot.
const linkedEventsCap = 500

// fetchLinked is the events every bucket linked into id adds to it, oldest first. A bucket
// that cannot be fetched adds nothing: the session itself is still worth showing.
func fetchLinked(ctx context.Context, c *apiclient.Client, windows []linkedWindow) []pipeline.SessionEvent {
	var out []pipeline.SessionEvent
	for _, w := range windows {
		view, err := c.GetSessionTail(ctx, w.bucket, linkedEventsCap)
		if err != nil || view == nil {
			continue
		}
		out = append(out, session.LinkedEvents(view.Events, w.after, w.before)...)
	}
	return out
}

// inWindow is the linked events a snapshot can hold: every one when the snapshot is the whole
// session, else those no older than its first event, which a later page would hold instead. A
// resumed session's linked calls are from the resume, so they are in its newest page.
func inWindow(linked []pipeline.SessionEvent, view *pipeline.SessionView) []pipeline.SessionEvent {
	if view.TotalEvents <= len(view.Events) || len(view.Events) == 0 {
		return linked
	}
	first := view.Events[0].At
	return slices.DeleteFunc(linked, func(e pipeline.SessionEvent) bool { return e.At.Before(first) })
}

// mergeLinked is a session's events with its linked calls among them by time, stable so a
// request still precedes its response where the two share a timestamp.
func mergeLinked(events, linked []pipeline.SessionEvent) []pipeline.SessionEvent {
	if len(linked) == 0 {
		return events
	}
	out := make([]pipeline.SessionEvent, 0, len(events)+len(linked))
	out = append(append(out, linked...), events...)
	slices.SortStableFunc(out, func(a, b pipeline.SessionEvent) int { return a.At.Compare(b.At) })
	return out
}

// eventSession is the session an event shown under sessionID was recorded in: its own
// SessionID when it came from a linked bucket, so the detail pane fetches it from there.
func eventSession(sessionID string, e *pipeline.SessionEvent) string {
	if e != nil && e.SessionID != "" && e.SessionID != sessionID {
		return e.SessionID
	}
	return sessionID
}
