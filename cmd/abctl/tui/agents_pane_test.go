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

// Rows order by cost descending, and ties break on the label.
//
// The tie-break is not cosmetic. These rows are rebuilt on every poll, and Go map iteration
// order is randomised per run — so two agents that cost the same would swap places between
// polls and the pane would flicker under a reader trying to compare them. rankSeriesByCost
// makes the same guarantee for the spend drawer and for the same reason; this is that rule
// applied to the surface where the reader is choosing a row to press Enter on.
//
// The zero-cost group is the case that matters for Bob specifically: until pricing lands, an
// unpriced agent's CostMicros is 0, so EVERY Bob row ties with every other unpriced agent and
// the tie-break is the only thing ordering them.
//
// ASSERTED ON sortAgentRows, NOT THROUGH THE FOLD, and the reason is measured rather than
// stylistic. Inside the fold the input arrives from a map walk that Go randomises per run, and
// sort.Slice is unstable — so a deleted tie-break survived 4 of 20 runs with two tied labels
// and 8 of 30 with four. Adding tied labels does not fix it, because the sort permutes the
// tied block itself. Handing the ordering a FIXED slice, already in the wrong order, makes the
// assertion deterministic: every run exercises the same permutation, so the guard either holds
// or fails, never sometimes.
func TestSortAgentRows_OrdersByCostThenLabel(t *testing.T) {
	// Deliberately the reverse of the wanted order within the tied group, so a missing
	// tie-break cannot coincidentally produce the right answer.
	rows := []agentRow{
		{label: "zeta/1.0", Counts: usage.Counts{Requests: 1, CostMicros: 0}},
		{label: "delta/1.0", Counts: usage.Counts{Requests: 1, CostMicros: 0}},
		{label: "beta/1.0", Counts: usage.Counts{Requests: 1, CostMicros: 0}},
		{label: "alpha/1.0", Counts: usage.Counts{Requests: 1, CostMicros: 0}},
		{label: "middle/1.0", Counts: usage.Counts{Requests: 1, CostMicros: 100}},
		{label: "claude-code/2.1.270", Counts: usage.Counts{Requests: 1, CostMicros: 500}},
	}

	sortAgentRows(rows)

	want := []string{
		"claude-code/2.1.270", "middle/1.0",
		"alpha/1.0", "beta/1.0", "delta/1.0", "zeta/1.0",
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for i, w := range want {
		if rows[i].label != w {
			got := make([]string, len(rows))
			for j, r := range rows {
				got[j] = r.label
			}
			t.Fatalf("row %d = %q, want %q\n  got order:  %v\n  want order: %v", i, rows[i].label, w, got, want)
		}
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
		// Usage is the one caller with a tail beyond the pane swap: its polling chain has to be
		// restarted on the way back, so without this row that branch is unexercised.
		{"opened from usage", paneUsage, paneUsage},
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

// The caller `esc` returns to is the pane `A` was pressed on, not the pane the reply lands on.
//
// THE FETCH IS A ROUND TRIP, so these are two different panes whenever the reader navigates
// while it is in flight — and `A` pressed ON the pane is the case that bit: it recorded
// paneAgents as its own caller, so the esc arm set pane to the pane it was already on and the
// key read as broken. agentRowsLoadedMsg.from carries the press-time answer across, which is the
// same reasoning the struct's `open` field already documents.
//
// Driven through handleKey and Update rather than by calling enterAgentsOrRefuse directly: the
// defect was in WHEN the caller was read, so a test that passes `from` itself cannot see it.
func TestAgentsPane_RecordsTheCallerAtPressTimeNotAtReplyTime(t *testing.T) {
	rows := []agentRow{
		{label: "claude-code/2.1.270", Counts: usage.Counts{Requests: 1049, PricedRequests: 1048}},
		{label: "bob-shell/2.0.5", Counts: usage.Counts{Requests: 8}},
	}
	for _, tc := range []struct {
		name string
		// pressedOn is where `A` is pressed; movedTo is where the reader has navigated to by
		// the time the reply lands. wantBack is where esc must then go.
		pressedOn, movedTo, wantBack paneID
		// previousPane seeds the model, so the refetch case can prove it is PRESERVED rather
		// than merely not overwritten with paneAgents.
		previousPane paneID
	}{
		// NOT paneSessions AS THE CALLER in either of the first two rows, and that is the
		// difference between a guard and a decoration. The esc arm falls back to paneSessions
		// when no caller was recorded, so a row that presses `A` FROM Sessions gets the right
		// answer out of the fallback as well — deleting the assignment outright left all three
		// rows green (mutant `enter-no-previouspane` SURVIVED) until these two moved off it. A
		// caller the fallback cannot coincide with is what makes the row able to fail.
		{
			name:      "reader stays put",
			pressedOn: paneDetail, movedTo: paneDetail, wantBack: paneDetail,
			previousPane: paneNone,
		},
		{
			// The reply-time read returned paneEvents here — the later pane, which never
			// asked for anything.
			name:      "reader navigates while the fetch is in flight",
			pressedOn: paneDetail, movedTo: paneEvents, wantBack: paneDetail,
			previousPane: paneNone,
		},
		{
			// A refetch. The reply-time read made the pane its own caller and esc went nowhere.
			name:      "A pressed again while already on the pane keeps the original caller",
			pressedOn: paneAgents, movedTo: paneAgents, wantBack: paneUsage,
			previousPane: paneUsage,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{
				pane:               tc.pressedOn,
				previousPane:       tc.previousPane,
				agents:             rows,
				agentsTbl:          newAgentsTable(),
				client:             deadClient(),
				pipelineReturnPane: paneNone,
			}
			// The press, and then its command is RUN, so the `from` under test is the one
			// keys.go resolved rather than one this test recomputed. deadClient points at a
			// refused port, so the reply carries an error — and `from` alongside it, which is
			// the only field wanted here. Recomputing the press-time rule locally would make
			// this pass no matter what keys.go decided.
			cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
			if cmd == nil {
				t.Fatalf("`A` on %v returned no fetch command", tc.pressedOn)
			}
			failed, ok := cmd().(agentRowsLoadedMsg)
			if !ok {
				t.Fatalf("`A`'s command did not produce an agentRowsLoadedMsg")
			}
			// The reader moves before the reply arrives. A reply-time read of m.pane sees this
			// pane; the press never did.
			m.pane = tc.movedTo
			updated, _ := m.Update(agentRowsLoadedMsg{rows: rows, open: true, from: failed.from})
			m = updated.(*model)
			if m.pane != paneAgents {
				t.Fatalf("reply did not open the pane: pane = %v", m.pane)
			}
			m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
			if m.pane != tc.wantBack {
				t.Errorf("esc after `A` from %v (reader moved to %v) landed on %v, want %v",
					tc.pressedOn, tc.movedTo, m.pane, tc.wantBack)
			}
		})
	}
}

// The COST cell says "—" for an agent nothing priced and a real figure for one priced at zero.
//
// THE ZERO-RATE ROW IS THE WHOLE TEST. "unpriced renders —" alone is satisfied by keying on
// CostMicros, which is the wrong field and the mistake this cell was written to avoid: a request
// CAN be priced at a rate of zero, and collapsing that into "not priced" throws away the one
// distinction the column exists to make. Only a row with PricedRequests > 0 AND CostMicros == 0
// separates the two readings, so without it both a CostMicros key and no guard at all pass.
//
// Bob is the live instance of the unpriced row — it bills in credits, which the cost model
// cannot represent — and `abctl cost --agent` makes the same call through snapshot.Priced, which
// is why TestRunCost_AgentReportsThatAgentOnly asserts "cost unavailable" on the same shape.
func TestAgentCostCell_UnpricedIsADashAndAZeroRateIsAFigure(t *testing.T) {
	for _, tc := range []struct {
		name string
		usage.Counts
		want string
	}{
		{
			// Bob: priceable traffic, nothing priced it.
			"nothing priced it",
			usage.Counts{Requests: 8, PriceableRequests: 7},
			emptyCell,
		},
		{
			// The case that fails a CostMicros key: priced, and it genuinely cost nothing.
			"priced at a rate of zero",
			usage.Counts{Requests: 4, PricedRequests: 4, PriceableRequests: 4},
			"$0.00",
		},
		{
			"priced with a cost",
			usage.Counts{Requests: 1049, PricedRequests: 1048, CostMicros: 146_361_600},
			"$146.36",
		},
		{
			// Sub-half-cent: the floor formatUSDTotalMicros carries, so a known charge never
			// prints as free. A dash here would be the same lie from the other direction.
			"priced below half a cent",
			usage.Counts{Requests: 1, PricedRequests: 1, CostMicros: 400},
			"<$0.01",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentCostCell(tc.Counts); got != tc.want {
				t.Errorf("agentCostCell(%+v) = %q, want %q", tc.Counts, got, tc.want)
			}
		})
	}
}

// And the cell the table actually renders is that function's answer, not a second spelling.
//
// agentCostCell could be correct while rebuildAgentsTable formatted the money itself — the
// mutation that deleted the guard would then still show $0.00 on screen with the unit test
// green. This walks the built rows so the assertion covers the path a reader sees.
func TestRebuildAgentsTable_RendersTheUnpricedAgentAsADash(t *testing.T) {
	m := &model{
		agentsTbl: newAgentsTable(),
		agents: []agentRow{
			{label: "claude-code/2.1.270", Counts: usage.Counts{
				Requests: 1049, Tokens: 297_961_318, PricedRequests: 1048, CostMicros: 146_361_600}},
			{label: "bob-shell/2.0.5", Counts: usage.Counts{
				Requests: 8, Tokens: 38_682, PriceableRequests: 7}},
		},
	}
	m.rebuildAgentsTable()
	rows := m.agentsTbl.Rows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Cost is the last column; see newAgentsTable for why it is the widest.
	const costCol = 3
	if got := rows[1][costCol]; got != emptyCell {
		t.Errorf("unpriced agent's COST cell = %q, want %q — $0.00 would read as free", got, emptyCell)
	}
	if got := rows[0][costCol]; got != "$146.36" {
		t.Errorf("priced agent's COST cell = %q, want %q", got, "$146.36")
	}
}
