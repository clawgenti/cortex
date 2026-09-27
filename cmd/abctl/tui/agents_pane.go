package tui

import (
	"context"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

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
// THE FOLD ITSELF IS usage.FoldSeriesAcrossWindow, not a loop here. It saturates through
// Counts.Add rather than wrapping, and `abctl cost --agent` needs the identical answer — two
// copies would be two definitions of what a window total means. The drawer's rankSeriesByCost
// records what the alternative cost when it did write its own: a wrapped total ranks BELOW a
// ten-micro series, which here would sort the busiest agent to the bottom of the table.
//
// ORDERED BY usage.SortSeriesLabels, not by a comparison written here. `abctl cost --by` ranks
// the same series for the same reason, so the rule has one definition in core and both surfaces
// call it — the tie-break on the label matters more than it sounds, because every unpriced agent
// has CostMicros 0 and until billing units land the label is the entire order for all of them.
// That function's godoc carries why it takes a slice rather than the map.
func agentRowsFromBuckets(buckets []usage.Bucket) []agentRow {
	totals := usage.FoldSeriesAcrossWindow(buckets)
	labels := make([]string, 0, len(totals))
	for label := range totals {
		labels = append(labels, label)
	}
	usage.SortSeriesLabels(labels, totals)
	out := make([]agentRow, 0, len(labels))
	for _, label := range labels {
		out = append(out, agentRow{label: label, Counts: totals[label]})
	}
	return out
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

// agentsPaneRefusal is why the pane will not open, or "" when it will.
//
// EXACTLY THE INVERSE OF agentsPaneApplies, pinned by
// TestAgentsPaneRefusal_AgreesWithAgentsPaneApplies. Two functions answering one question is
// how the spend drawer came to print the wrong refusal on two panes: the decision and the
// sentence explaining it drifted. They are separate here only because one is a branch and the
// other is prose, and the test makes the agreement the compiler's-equivalent of enforced.
//
// A REFUSAL MAY NEVER BE SILENT, which is the contract spendDrawerHostPane keeps and which
// this package has twice been bitten by breaking. `A` doing nothing looks like a broken
// binding, and a reader cannot tell that from a pane deciding it had nothing worth showing.
//
// The two cases get DIFFERENT SENTENCES because they are different situations: no agents means
// nothing has been observed yet and waiting may fix it; one agent means the breakdown would
// restate a total the reader already has, and waiting will not change that until a second
// agent appears.
func agentsPaneRefusal(rows []agentRow) string {
	switch len(rows) {
	case 0:
		return "agents: no agent traffic seen in this window yet"
	case 1:
		// Names the agent, so it is visible that a per-agent breakdown would be one row
		// repeating the figure already on screen.
		return "agents: only " + rows[0].label + " has been seen — a breakdown would be one row"
	}
	return ""
}

// agentsFetchTimeout bounds the one request this pane makes. The same 5s the usage pane
// allows itself, matched rather than chosen again: both call GetUsage against the same
// endpoint, and two different bounds on one call would be two different answers to "how long
// before we give up".
const agentsFetchTimeout = 5 * time.Second

// agentsWindow is the span the breakdown covers.
//
// A SYMBOLIC WINDOW, so the figures come from the durable cost ledger rather than the
// six-hour ring. "Which agents have spent what" is a question about a day, and the ring cannot
// answer it — a reader comparing this against `abctl cost` (which defaults to the same window)
// must not find two different denominators. Where the ledger is off the proxy serves the
// longest window it holds and says so in the response, which is the same degradation
// `abctl cost` documents.
const agentsWindow = usage.WindowToday

// agentRowsLoadedMsg carries a fetched per-agent breakdown back to Update.
//
// NOT agentsLoadedMsg, which is TAKEN — by the Kubernetes namespace picker, whose
// Lister.ListAgents lists agent WORKLOADS. Same word, unrelated meaning; see paneAgents.
type agentRowsLoadedMsg struct {
	rows []agentRow
	err  error
	// open records that the `A` key asked for this, so the reply may enter the pane. A
	// background refresh sets it false and only updates the table, which is why this is a
	// field rather than inferred from the current pane: by the time a reply lands the reader
	// may have moved.
	open bool
}

// fetchAgentRowsCmd requests the per-agent breakdown off the render loop.
//
// group=agent AND NO AGENT FILTER, because /v1/usage has none: it reads window, resolution,
// group and session, and session is its only scoping parameter. The per-agent split therefore
// arrives as Bucket.Series and is folded here. That limit is also why this pane is read-only —
// there is no server-side agent scope to apply to any other pane.
func (m *model) fetchAgentRowsCmd(open bool) tea.Cmd {
	if m.client == nil {
		return nil
	}
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), agentsFetchTimeout)
		defer cancel()
		snap, err := client.GetUsageWindow(ctx, agentsWindow, 0, "", usage.GroupAgent)
		if err != nil {
			return agentRowsLoadedMsg{err: err, open: open}
		}
		return agentRowsLoadedMsg{rows: agentRowsFromBuckets(snap.Buckets), open: open}
	}
}

// newAgentsTable builds an empty per-agent breakdown table.
//
// NO SESSIONS COLUMN — see agentRow. COST is widest because it is the column the pane exists
// for, and it holds "—" for an agent nothing could price, which is every Bob row until the
// billing-unit work lands.
func newAgentsTable() table.Model {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "AGENT", Width: 34},
			{Title: "REQUESTS", Width: 10},
			{Title: "TOKENS", Width: 10},
			{Title: "COST", Width: 12},
		}),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildAgentsTable rebuilds rows from m.agents.
func (m *model) rebuildAgentsTable() {
	rows := make([]table.Row, 0, len(m.agents))
	for _, a := range m.agents {
		rows = append(rows, table.Row{
			// SANITISED AT RENDER TIME. The label is a User-Agent, so it is
			// request-controlled; tui.sanitizeLabel is the package's render-time copy of the
			// rule, named as such in ledger's own comment.
			sanitizeLabel(a.label),
			formatCount(int(a.Requests)),
			humanizeCount(a.Tokens),
			agentCostCell(a.Counts),
		})
	}
	m.agentsTbl.SetRows(rows)
}

// agentCostCell renders one agent's cost, or "—" when nothing priced it.
//
// "—" AND NEVER "$0.00", which is this codebase's standing rule and the reason
// SessionSummary.CostMicros is omitempty: an agent whose traffic nothing could price is not an
// agent that spent nothing. Bob is exactly that case today — it bills in credits, which the
// cost model cannot yet represent — so every Bob row reads "—" rather than claiming it was
// free.
//
// PricedRequests is the test, not CostMicros, because a genuine zero is possible: a request
// can be priced at a rate of zero. Reading the money field instead would collapse "priced, and
// it cost nothing" into "not priced".
func agentCostCell(c usage.Counts) string {
	if c.PricedRequests == 0 {
		return emptyCell
	}
	// formatUSDTotalMicros, not %.2f over micros/1e6: it does the rounding on the integer, so
	// 1_005_000 micros renders $1.01 rather than the $1.00 a float64 %.2f produces. It also
	// carries the floor that keeps a known sub-cent charge from printing as free.
	return formatUSDTotalMicros(c.CostMicros)
}

// enterAgentsOrRefuse opens the pane, or returns the reason it will not.
//
// ONE DECISION POINT for both callers — the key press that already has rows, and the reply
// that has just fetched them — so the two cannot drift on what counts as available. The
// refusal string is agentsPaneRefusal's, never rephrased here.
func (m *model) enterAgentsOrRefuse() (entered bool, refusal string) {
	if why := agentsPaneRefusal(m.agents); why != "" {
		return false, why
	}
	m.previousPane = m.pane
	m.pane = paneAgents
	m.rebuildAgentsTable()
	return true, ""
}
