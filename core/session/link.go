package session

import (
	"slices"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// PendingLink is one session a pending bucket's process named, with the span of its naming
// requests: the first and the last. Which of a bucket's calls are whose is OwnerAt's answer.
type PendingLink struct {
	Session string    `json:"session"`
	First   time.Time `json:"first"`
	Last    time.Time `json:"last"`
}

// OwnerAt is the session a call its process made at t belongs to, "" for none: the first session
// the process named at or after t. Concretely, among the links whose span has not ended by t
// (Last >= t), the one whose naming resumed soonest — a span already covering t resumed "at t",
// so it beats one that begins later, and between two that both cover t the one begun more
// recently is the one the process had just named.
//
// LAST DECIDES ELIGIBILITY, FIRST DECIDES ORDER. Ordering by Last instead sent an OpenCode
// service's start-up calls into a session it began naming minutes after them, because that
// session's span happened to end before the first one's. Eligibility by Last is what keeps the
// restart case: a session named yesterday and again today, after a restart lost the claim,
// still takes the calls made today before it was named again.
func OwnerAt(links []PendingLink, t time.Time) string {
	best, bestAt := "", time.Time{}
	for _, l := range links {
		if l.Last.Before(t) {
			continue
		}
		at := l.First
		if at.Before(t) {
			at = t
		}
		if best == "" || at.Before(bestAt) || (at.Equal(bestAt) && l.First.After(firstOf(links, best))) {
			best, bestAt = l.Session, at
		}
	}
	return best
}

func firstOf(links []PendingLink, id string) time.Time {
	for _, l := range links {
		if l.Session == id {
			return l.First
		}
	}
	return time.Time{}
}

// Link marks every pending bucket in list whose calls a process made before naming a
// session: LinkedTo is the sessions its process named — no earlier than the bucket's first
// call, ordered by First — and LinkedAll says every request in the bucket has an owner
// (OwnerAt), which holds when the newest request is no later than some link's Last. It is how
// a process's pre-session calls reach their session now that nothing renames a bucket into
// one: computed from what is recorded, each time the list is read, so it survives a restart
// and needs no merge. A response goes where its request went, which is why the bucket's figure
// is its newest REQUEST (LastRequestAt).
//
// Only a per-process bucket links. The agent-wide pending:<agent> — a client the lookup could
// not name — and the default bucket carry no process, and linking them would be a guess.
//
// list is modified in place and returned. O(sessions × procs); called once per list request.
func Link(list []SessionSummary) []SessionSummary {
	byProc := map[string][]PendingLink{}
	for i := range list {
		for p, sp := range list[i].procSpans {
			byProc[p] = append(byProc[p], PendingLink{Session: list[i].ID, First: sp.First, Last: sp.Last})
		}
	}
	if len(byProc) == 0 {
		return list
	}
	for i := range list {
		_, proc, ok := ParsePending(list[i].ID)
		if !ok || proc == "" {
			continue
		}
		var to []PendingLink
		var latest time.Time
		for _, l := range byProc[proc] {
			if l.Last.Before(list[i].CreatedAt) {
				continue
			}
			to = append(to, l)
			if l.Last.After(latest) {
				latest = l.Last
			}
		}
		if len(to) == 0 {
			continue
		}
		slices.SortFunc(to, func(a, b PendingLink) int { return a.First.Compare(b.First) })
		list[i].LinkedTo = to
		list[i].LinkedAll = !latest.Before(list[i].LastRequestAt)
	}
	return list
}

// ParsePending splits a pending bucket's id into its agent and process ("<pid>.<start>", ""
// for the agent-wide bucket), and reports false for any other id.
func ParsePending(id string) (agent, proc string, ok bool) {
	rest, ok := strings.CutPrefix(id, PendingPrefix)
	if !ok || rest == "" {
		return "", "", false
	}
	agent, proc, _ = strings.Cut(rest, "@")
	return agent, proc, agent != ""
}

// LinkedEvents is the part of a linked bucket's events that belongs to session: the requests
// OwnerAt gives it, each with its response, wherever the response landed in time.
func LinkedEvents(events []pipeline.SessionEvent, links []PendingLink, session string) []pipeline.SessionEvent {
	keep := map[string]bool{}
	out := events[:0:0]
	for i := range events {
		e := &events[i]
		if e.Phase == pipeline.SessionRequest {
			if OwnerAt(links, e.At) != session {
				continue
			}
			if e.RequestID != "" {
				keep[e.RequestID] = true
			}
		} else if e.RequestID == "" || !keep[e.RequestID] {
			continue
		}
		out = append(out, *e)
	}
	return out
}
