package tui

import (
	"sort"

	"github.com/rossoctl/cortex/core/cost/usage"
)

// agentRow is one coding agent's totals for the whole window, as the AGENTS pane shows them.
//
// The label is pipeline.EventClient.Label — "claude-code/2.1.270", or the raw User-Agent for
// an agent the parser did not recognise. CLIENT-ASSERTED AND SPOOFABLE, like every other use
// of that field: a display and scoping key, never an authorization subject.
//
// NO SESSIONS COUNT, and that absence is deliberate rather than pending. session.SessionSummary
// carries no agent, and the cost ledger's key is (endpoint, model, agent, provenance) with no
// session dimension BY DESIGN — so "how many sessions did this agent have" is not a question
// anything on the wire can answer, and a column here would have to invent it.
type agentRow struct {
	label string
	usage.Counts
}

// agentRowsFromBuckets folds a snapshot's per-agent series into one row per agent.
//
// The snapshot carries per-agent figures PER BUCKET, while this pane shows one row per agent
// for the window, so this fold is the pane's whole data path. Callers pass the buckets from a
// snapshot fetched with usage.GroupAgent; any other grouping yields rows labelled by that
// grouping's keys instead, which is the caller's error to avoid and not something this can
// detect — Bucket.Series does not record which axis produced it.
//
// MONEY AND COUNTERS THROUGH usage.Counts.Add, never a raw `+=`. Add saturates and records it
// in Saturated, and it is exported precisely so abctl folds arbitrary Counts the one way. The
// drawer's rankSeriesByCost carries the full argument: a wrapped total ranks BELOW a ten-micro
// series, which here would sort the busiest agent to the bottom of the picker.
//
// ORDERED BY COST DESCENDING, TIES ON LABEL. The tie-break is load-bearing, not tidiness:
// these rows are rebuilt on every poll and Go randomises map iteration per run, so equal-cost
// agents would swap places between polls under a reader comparing them. It matters most right
// now — every unpriced agent has CostMicros 0, so until pricing lands the label IS the order
// for all of them.
func agentRowsFromBuckets(buckets []usage.Bucket) []agentRow {
	totals := map[string]*usage.Counts{}
	for _, b := range buckets {
		for label, c := range b.Series {
			if totals[label] == nil {
				totals[label] = &usage.Counts{}
			}
			totals[label].Add(c)
		}
	}
	out := make([]agentRow, 0, len(totals))
	for label, c := range totals {
		out = append(out, agentRow{label: label, Counts: *c})
	}
	sortAgentRows(out)
	return out
}

// sortAgentRows orders rows by cost descending, breaking ties on the label.
//
// SEPARATE FROM THE FOLD so the ordering can be asserted deterministically. Inside the fold
// its input arrives from a map walk, which Go randomises per run, and sort.Slice is not
// stable — so a test that fed the fold tied labels could only catch a missing tie-break when
// the random order happened to be wrong. Measured before this split: a deleted tie-break
// survived 6 runs in 30 with four tied labels, and 4 in 20 with two. A guard that misses a
// real defect one run in four reads as coverage and is not.
//
// Given a slice, the order is a pure function of it, so a test can hand this a deliberately
// mis-ordered input and the assertion holds every run.
func sortAgentRows(rows []agentRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CostMicros != rows[j].CostMicros {
			return rows[i].CostMicros > rows[j].CostMicros
		}
		return rows[i].label < rows[j].label
	})
}

// agentsPaneApplies reports whether the AGENTS pane is worth entering.
//
// FEWER THAN TWO AGENTS SKIPS THE PANE, and this is the property that keeps the feature from
// costing every existing user something. A picker offering one row is a keystroke that cannot
// change what is displayed, and one agent is EVERY deployment today — so entering
// unconditionally would put a new mandatory step in front of everyone to no purpose. Mirrors
// the Namespaces → Pods picker, which is likewise conditional.
//
// ZERO SKIPS TOO, and that case is reachable rather than theoretical: a proxy that has served
// no inference yet reports no agent series, and an empty picker with nothing to select is a
// worse answer than going straight to the view.
//
// A RULE OVER DATA rather than a branch inside the navigation code, so it is assertable
// without driving the TUI and cannot be satisfied by an incidental detail of how panes happen
// to be entered.
func agentsPaneApplies(rows []agentRow) bool {
	return len(rows) >= 2
}
