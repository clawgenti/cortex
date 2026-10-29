package tui

import (
	"context"
	"slices"

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

// linkedBucket is one bucket that feeds a session, with that bucket's links (OwnerAt decides
// which of its calls are the session's).
type linkedBucket struct {
	bucket string
	links  []session.PendingLink
}

// linkedInto is every bucket whose links name id.
func (m *model) linkedInto(id string) []linkedBucket {
	var out []linkedBucket
	for _, s := range m.sessions {
		if slices.ContainsFunc(s.LinkedTo, func(l session.PendingLink) bool { return l.Session == id }) {
			out = append(out, linkedBucket{bucket: s.ID, links: s.LinkedTo})
		}
	}
	return out
}

// foldedInto is the session a bucket is folded into, "" when it is not: a bucket every request
// of which belongs to one session (LinkedAll, one link). Its row is hidden, its figures join
// that session's row, and erasing the session erases it. A bucket linked to two sessions — a
// /clear in its middle — stays a row: its calls and spend cannot be given to one of them.
func foldedInto(s session.SessionSummary) string {
	if !s.LinkedAll || len(s.LinkedTo) != 1 {
		return ""
	}
	return s.LinkedTo[0].Session
}

// foldedFigures is what the buckets folded into id add to its row: cost, savings and tokens.
func (m *model) foldedFigures(id string) (cost, avoided int64, tokens int) {
	for _, s := range m.sessions {
		if foldedInto(s) == id {
			cost += s.CostMicros
			avoided += s.AvoidedMicros
			tokens += s.TotalTokens
		}
	}
	return cost, avoided, tokens
}

// withFolded is ids plus every bucket folded into one of them, for an erase.
func (m *model) withFolded(ids []string) []string {
	out := slices.Clone(ids)
	for _, s := range m.sessions {
		if to := foldedInto(s); to != "" && slices.Contains(ids, to) && !slices.Contains(out, s.ID) {
			out = append(out, s.ID)
		}
	}
	return out
}

// linkedEventsCap bounds what one session's linked buckets add to its first snapshot. A bucket
// is a process's start-up calls, a dozen events; this only stops a chatty process (an SDK
// probe loop) from doubling a snapshot.
const linkedEventsCap = 500

// fetchLinked is the events every bucket linked into id adds to it, oldest first. A bucket
// that cannot be fetched adds nothing: the session itself is still worth showing.
func fetchLinked(ctx context.Context, c *apiclient.Client, id string, buckets []linkedBucket) []pipeline.SessionEvent {
	var out []pipeline.SessionEvent
	for _, b := range buckets {
		view, err := c.GetSessionTail(ctx, b.bucket, linkedEventsCap)
		if err != nil || view == nil {
			continue
		}
		out = append(out, session.LinkedEvents(view.Events, b.links, id)...)
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

// pageLinked is an older page with the linked calls inside its span merged in. oldest says the
// page is the session's first, which also takes every linked call before it: a fresh launch's
// handshake precedes the session's first request.
func pageLinked(page, linked []pipeline.SessionEvent, oldest bool) []pipeline.SessionEvent {
	if len(page) == 0 || len(linked) == 0 {
		return page
	}
	from, to := page[0].At, page[len(page)-1].At
	in := slices.DeleteFunc(slices.Clone(linked), func(e pipeline.SessionEvent) bool {
		return e.At.After(to) || (!oldest && e.At.Before(from))
	})
	return mergeLinked(page, in)
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

// ownOldest is the oldest of held that sessionID recorded itself, nil when none is: paging
// counts from it, because a linked bucket's events are numbered in their own bucket.
func ownOldest(held []pipeline.SessionEvent, sessionID string) *pipeline.SessionEvent {
	for i := range held {
		if eventSession(sessionID, &held[i]) == sessionID {
			return &held[i]
		}
	}
	return nil
}
