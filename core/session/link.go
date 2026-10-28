package session

import (
	"slices"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// PendingLink is one session a pending bucket's calls belong to: those made after the
// previous link's Until (the bucket's start for the first) and at or before this one's.
type PendingLink struct {
	Session string    `json:"session"`
	Until   time.Time `json:"until"`
}

// Link marks every pending bucket in list whose calls a process made before naming a
// session: LinkedTo is those sessions, in the order of the process's claims, and LinkedAll
// says every request in the bucket falls before one of them. It is how a process's
// pre-session calls reach their session now that nothing renames a bucket into one — the
// answer is computed from what is recorded, each time the list is read, so it survives a
// restart and needs no merge.
//
// THE RULE: an event in process P's bucket belongs to the first session P named at or after
// the event's time. A response goes where its request went, which is why a bucket's figure
// is its newest REQUEST (LastRequestAt): a stream opened before the claim and closed after it
// still belongs before the claim. "Named at or after" reads each session's span for P by its
// LAST naming request, not its first: a resumed session P named yesterday, then names again
// today after a restart lost the claim, still takes the calls P made today before naming it.
//
// Only a per-process bucket links. The agent-wide pending:<agent> — a client the lookup could
// not name — and the default bucket carry no process, and linking them would be a guess.
//
// list is modified in place and returned. O(sessions × procs), with no allocation for a
// session that no process named; called once per list request.
func Link(list []SessionSummary) []SessionSummary {
	type claim struct {
		id   string
		last time.Time
	}
	byProc := map[string][]claim{}
	for i := range list {
		for p, sp := range list[i].procSpans {
			byProc[p] = append(byProc[p], claim{list[i].ID, sp.Last})
		}
	}
	if len(byProc) == 0 {
		return list
	}
	for p := range byProc {
		slices.SortFunc(byProc[p], func(a, b claim) int { return a.last.Compare(b.last) })
	}
	for i := range list {
		_, proc, ok := ParsePending(list[i].ID)
		if !ok || proc == "" {
			continue
		}
		claims := byProc[proc]
		if len(claims) == 0 {
			continue
		}
		var to []PendingLink
		for _, c := range claims {
			if c.last.Before(list[i].CreatedAt) {
				continue
			}
			if n := len(to); n > 0 && to[n-1].Session == c.id {
				to[n-1].Until = c.last
				continue
			}
			to = append(to, PendingLink{Session: c.id, Until: c.last})
		}
		if len(to) == 0 {
			continue
		}
		list[i].LinkedTo = to
		// Every request falls before a claim when the newest one does.
		list[i].LinkedAll = !claims[len(claims)-1].last.Before(list[i].LastRequestAt)
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

// LinkedEvents is the part of a linked bucket's events that one of its links takes: requests
// after after and at or before before, each with its response, wherever the response landed
// in time. after is the previous link's Until, zero for the first link.
func LinkedEvents(events []pipeline.SessionEvent, after, before time.Time) []pipeline.SessionEvent {
	keep := map[string]bool{}
	out := events[:0:0]
	for i := range events {
		e := &events[i]
		if e.Phase == pipeline.SessionRequest {
			if e.At.After(before) || (!after.IsZero() && !e.At.After(after)) {
				continue
			}
			keep[e.RequestID] = true
		} else if !keep[e.RequestID] {
			continue
		}
		out = append(out, *e)
	}
	return out
}
