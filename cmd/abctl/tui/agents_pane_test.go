package tui

import (
	"math"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/cost/usage"
)

// One agent's totals are folded across every bucket in the window.
//
// The snapshot carries per-agent figures PER BUCKET (usage.Bucket.Series), while the pane
// shows one row per agent for the whole window — so the fold is the pane's entire data path,
// and a fold that took only the last bucket would still render a plausible-looking table.
// Two buckets for one agent is the smallest input that tells the difference.
func TestAgentRowsFromBuckets_FoldsOneAgentAcrossBuckets(t *testing.T) {
	buckets := []usage.Bucket{
		{Series: map[string]usage.Counts{
			"bob-shell/2.0.5": {Requests: 3, Tokens: 1_000, CostMicros: 2_000},
		}},
		{Series: map[string]usage.Counts{
			"bob-shell/2.0.5": {Requests: 5, Tokens: 2_500, CostMicros: 5_000},
		}},
	}

	rows := agentRowsFromBuckets(buckets)

	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 — one agent must fold to one row", len(rows))
	}
	if rows[0].label != "bob-shell/2.0.5" {
		t.Errorf("label = %q, want %q", rows[0].label, "bob-shell/2.0.5")
	}
	if rows[0].Requests != 8 {
		t.Errorf("Requests = %d, want 8 (3+5)", rows[0].Requests)
	}
	if rows[0].Tokens != 3_500 {
		t.Errorf("Tokens = %d, want 3500 (1000+2500)", rows[0].Tokens)
	}
	if rows[0].CostMicros != 7_000 {
		t.Errorf("CostMicros = %d, want 7000 (2000+5000)", rows[0].CostMicros)
	}
}

// The fold puts the most expensive agent first.
//
// Ordering by cost is deterministic even out of a map walk, because the costs differ — so this
// is the half of the contract the fold itself can honestly assert. The tie-break is pinned by
// TestSortAgentRows_OrdersByCostThenLabel, which does not depend on map order at all.
func TestAgentRowsFromBuckets_PutsTheCostliestAgentFirst(t *testing.T) {
	buckets := []usage.Bucket{{Series: map[string]usage.Counts{
		"cheap/1.0":           {Requests: 1, CostMicros: 1},
		"claude-code/2.1.270": {Requests: 1, CostMicros: 500},
		"middle/1.0":          {Requests: 1, CostMicros: 100},
	}}}

	rows := agentRowsFromBuckets(buckets)

	want := []string{"claude-code/2.1.270", "middle/1.0", "cheap/1.0"}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for i, w := range want {
		if rows[i].label != w {
			t.Errorf("row %d = %q, want %q", i, rows[i].label, w)
		}
	}
}

// The AGENTS pane is skipped below two agents.
//
// This is a user-experience requirement with a correctness edge: a picker offering one choice
// is a keystroke that cannot change anything, and every deployment today has exactly one
// agent — so entering the pane unconditionally would put a new mandatory step in front of
// every existing user to no purpose. It is asserted as a RULE OVER DATA rather than through
// the TUI so that it cannot be satisfied by some incidental navigation detail.
//
// Zero is included and is not hypothetical: a proxy that has served no inference yet reports
// no agent series at all, and that must behave like the single-agent case rather than
// entering an empty picker with nothing to select.
func TestAgentsPaneApplies_SkippedBelowTwoAgents(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rows  []agentRow
		apply bool
	}{
		{"no agents yet", nil, false},
		{"one agent, which is every deployment today", []agentRow{{label: "claude-code/2.1.270"}}, false},
		{"two agents is the case the pane exists for", []agentRow{
			{label: "claude-code/2.1.270"}, {label: "bob-shell/2.0.5"},
		}, true},
		{"three agents", []agentRow{
			{label: "a"}, {label: "b"}, {label: "c"},
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentsPaneApplies(tc.rows); got != tc.apply {
				t.Errorf("agentsPaneApplies(%d rows) = %v, want %v", len(tc.rows), got, tc.apply)
			}
		})
	}
}

// Refusing the pane says WHY, and the reason names the actual count.
//
// A key that does nothing is the failure this package has been bitten by twice — paneUsage
// shipped reachable and undocumented, and the spend drawer once printed the wrong refusal
// reason on two panes. So `A` below two agents must not be silently inert: it refuses and
// says what it found, the same contract spendDrawerHostPane keeps, whose test requires that
// no refusal be silent.
//
// The two refusals are DIFFERENT SENTENCES because they are different situations: no agents
// means nothing has been observed yet and waiting may fix it, while one agent means the
// breakdown would have a single row and waiting will not. Collapsing them into "not enough
// agents" tells a reader nothing about which of those they are looking at.
func TestAgentsPaneRefusal_NamesWhyAndIsSilentOnlyWhenAvailable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rows     []agentRow
		wantSome bool     // a refusal is expected
		contains []string // fragments the refusal must carry
	}{
		{
			name: "no agents seen yet", rows: nil, wantSome: true,
			contains: []string{"no agent"},
		},
		{
			name: "one agent", rows: []agentRow{{label: "claude-code/2.1.270"}}, wantSome: true,
			// The agent's own name, so the reader can see the breakdown would be a
			// restatement of the total they already have.
			contains: []string{"claude-code/2.1.270"},
		},
		{
			name: "two agents is available", rows: []agentRow{
				{label: "claude-code/2.1.270"}, {label: "bob-shell/2.0.5"},
			}, wantSome: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := agentsPaneRefusal(tc.rows)
			if !tc.wantSome {
				if got != "" {
					t.Fatalf("agentsPaneRefusal = %q, want empty — the pane is available here", got)
				}
				return
			}
			if got == "" {
				t.Fatal("agentsPaneRefusal = empty; a refusal may never be silent")
			}
			for _, frag := range tc.contains {
				if !strings.Contains(got, frag) {
					t.Errorf("refusal %q does not mention %q", got, frag)
				}
			}
		})
	}
}

// The refusal and the availability rule can never disagree.
//
// Two functions answering one question is how the spend drawer's wrong-reason bug happened:
// the decision and the sentence explaining it drifted apart. Asserted over both sides of the
// boundary rather than at it, so a change to either that forgets the other fails here.
func TestAgentsPaneRefusal_AgreesWithAgentsPaneApplies(t *testing.T) {
	for n := 0; n <= 3; n++ {
		rows := make([]agentRow, n)
		for i := range rows {
			rows[i] = agentRow{label: string(rune('a' + i))}
		}
		applies := agentsPaneApplies(rows)
		refused := agentsPaneRefusal(rows) != ""
		if applies == refused {
			t.Errorf("%d agents: agentsPaneApplies=%v but refused=%v — these must be exact opposites",
				n, applies, refused)
		}
	}
}

// The help overlay tells the two "agent" panes apart.
//
// This repo uses the word for two unrelated things: paneNamespaces lists KUBERNETES
// workloads, and its purpose line called them "agents grouped by namespace", while paneAgents
// lists CODING agents — the clients seen on the wire. Side by side in the [?] overlay those
// two descriptions sent a reader to the wrong pane, and the overlay is the one surface that
// renders both at once, so it is where the ambiguity had to be resolved.
//
// Asserted on the distinguishing WORD in each, not on the full sentence, so rewording either
// purpose stays free while dropping the distinction does not.
func TestPaneKeys_TheTwoAgentPanesAreDistinguishable(t *testing.T) {
	ns, ok := paneKeys[paneNamespaces]
	if !ok {
		t.Fatal("paneNamespaces has no paneKeys entry")
	}
	ag, ok := paneKeys[paneAgents]
	if !ok {
		t.Fatal("paneAgents has no paneKeys entry")
	}
	if !strings.Contains(ns.purpose, "Kubernetes") {
		t.Errorf("paneNamespaces purpose %q does not say Kubernetes — it lists workloads, and without that word it reads as the coding-agent pane", ns.purpose)
	}
	if !strings.Contains(ag.purpose, "coding") {
		t.Errorf("paneAgents purpose %q does not say coding — it lists wire clients, and without that word it reads as the namespace pane", ag.purpose)
	}
}

// Money folds through usage.Counts.Add, so it saturates instead of wrapping negative.
//
// Not a figure anybody will reach — it needs ~$9.2T in one label — but the direction of the
// failure is what makes it worth pinning, and the package has already been bitten by it: the
// spend drawer summed with a raw accumulator, and rankSeriesByCost's godoc records that a
// wrapped total ranks BELOW a ten-micro series, which silently drops the window's most
// expensive row out of the table. Here the same wrap would put the busiest agent at the
// bottom of the picker. Add is exported precisely so abctl folds arbitrary Counts the one
// way, and this asserts this pane took it.
func TestAgentRowsFromBuckets_MoneySaturatesRatherThanWrapping(t *testing.T) {
	buckets := []usage.Bucket{
		{Series: map[string]usage.Counts{"a/1": {CostMicros: math.MaxInt64 - 5}}},
		{Series: map[string]usage.Counts{"a/1": {CostMicros: 100}}},
	}

	rows := agentRowsFromBuckets(buckets)

	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].CostMicros != math.MaxInt64 {
		t.Errorf("CostMicros = %d, want MaxInt64 — a raw += would have wrapped negative here", rows[0].CostMicros)
	}
	if !rows[0].Saturated {
		t.Error("Saturated = false; the row must disclose that its total was clamped")
	}
}

// esc leaves the AGENTS pane and lands where `A` was pressed.
//
// A KEY-OPENED SURFACE MUST RETURN TO ITS CALLER, which is the rule paneCatalog's own esc
// case states and the reason the sessions pane is the only one whose esc costs the
// connection. Without a case of its own a new pane is a DEAD END: esc falls through, the
// reader is stuck, and the only way out is `q`.
//
// The paneNone fallback is Sessions rather than the pane enum's zero value. It is reachable
// rather than defensive — the same way paneCatalog's is — and Sessions is the one pane that is
// always a defensible place to land; falling back to the zero value would drop the reader on
// the Kubernetes namespace picker, tearing down nothing but looking like the connection went
// away.
func TestAgentsPane_EscReturnsToTheCaller(t *testing.T) {
	for _, tc := range []struct {
		name         string
		previousPane paneID
		want         paneID
	}{
		{"opened from sessions", paneSessions, paneSessions},
		{"opened from events", paneEvents, paneEvents},
		{"opened from detail", paneDetail, paneDetail},
		{"no caller recorded falls back to sessions", paneNone, paneSessions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{
				pane:               paneAgents,
				previousPane:       tc.previousPane,
				client:             deadClient(),
				pipelineReturnPane: paneNone,
			}
			m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
			if m.pane != tc.want {
				t.Errorf("esc from AGENTS left pane = %v, want %v", m.pane, tc.want)
			}
		})
	}
}

// An agent nothing could price renders "—", and a genuine zero renders "$0.00".
//
// THE TWO READINGS THIS SEPARATES are the whole reason agentCostCell tests PricedRequests rather
// than CostMicros: "nothing priced this agent" and "this agent was priced, and it cost nothing"
// are different answers, and only the first is unknown. A guard written on the money field would
// collapse them, and so would a test that only banned the string "$0.00" — the genuine-zero row
// below is what makes this able to tell a correct implementation from that one.
//
// The CLI twin of this rule is pinned by TestRunCost_ByRendersUnpricedAsADashNotZero. This is the
// TUI half, which the AGENTS pane's COST column exists for.
func TestAgentCostCell_UnpricedIsADashAndAGenuineZeroIsNot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts usage.Counts
		want   string
	}{
		{"nothing priced", usage.Counts{Requests: 8, PriceableRequests: 7}, emptyCell},
		{"priced at a rate of zero", usage.Counts{Requests: 4, PricedRequests: 4, CostMicros: 0}, "$0.00"},
		{"priced and charged", usage.Counts{Requests: 4, PricedRequests: 4, CostMicros: 1_500_000}, "$1.50"},
		// Sub-cent, because the column's own comment says formatUSDTotalMicros "carries the floor
		// that keeps a known sub-cent charge from printing as free". Note the third distinct
		// answer: a known charge under a cent is "<$0.01", which is neither the "$0.00" of a
		// genuine zero nor the "—" of an unpriced agent. All three readings stay separable.
		{"priced below a cent", usage.Counts{Requests: 1, PricedRequests: 1, CostMicros: 400}, "<$0.01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentCostCell(tc.counts); got != tc.want {
				t.Errorf("agentCostCell(%+v) = %q, want %q", tc.counts, got, tc.want)
			}
		})
	}
}

// The rule survives the trip through the table the pane actually renders.
//
// agentCostCell is correct in isolation above; this pins that rebuildAgentsTable puts its output
// in the COST cell rather than formatting the money a second way. Two rows, one priced and one
// not, so a builder that dropped the helper would have to reproduce both answers to pass.
func TestRebuildAgentsTable_CarriesTheCostCellRuleIntoTheRow(t *testing.T) {
	m := &model{
		agentsTbl: newAgentsTable(),
		agents: []agentRow{
			{label: "claude-code/2.1.270", Counts: usage.Counts{Requests: 9, PricedRequests: 9, CostMicros: 2_250_000}},
			{label: "bob-shell/2.0.5", Counts: usage.Counts{Requests: 8, PriceableRequests: 7}},
		},
	}
	m.rebuildAgentsTable()

	rows := m.agentsTbl.Rows()
	if len(rows) != 2 {
		t.Fatalf("rebuilt %d rows, want 2", len(rows))
	}
	// Column 3 is COST — see newAgentsTable's column list.
	if got, want := rows[0][3], "$2.25"; got != want {
		t.Errorf("priced row COST = %q, want %q", got, want)
	}
	if got, want := rows[1][3], emptyCell; got != want {
		t.Errorf("unpriced row COST = %q, want %q (never $0.00)", got, want)
	}
}
