package usage

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// agent= narrows the ring to one agent: its totals, its units, and the echo that tells a client
// the filter was applied.
func TestAgentSnapshot_NarrowsTheRingToOneAgent(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := twoUnitRing(t, now)
	// The ring records Bob under the pre-convergence spelling "bob-shell" (see
	// twoUnitRing), and the query names today's "bob" — the AgentName fold is what
	// joins them, which is the production situation for every stored row.
	const bob = "bob"

	snap := a.AgentSnapshot(10*BucketWidth, BucketWidth, "", bob, GroupNone)
	if snap.Agent != bob {
		t.Errorf("Agent = %q, want the %q echo", snap.Agent, bob)
	}
	if snap.Totals.CostMicros != 7_800 {
		t.Errorf("Totals.CostMicros = %d, want Bob's 7800 alone", snap.Totals.CostMicros)
	}
	if !slices.Equal(snap.Currencies, []string{"Bobcoins"}) {
		t.Errorf("Currencies = %v, want [Bobcoins]: Claude's dollars are not Bob's", snap.Currencies)
	}

	idle := a.AgentSnapshot(10*BucketWidth, BucketWidth, "", "nobody/1.0", GroupNone)
	if idle.Agent != "nobody/1.0" || idle.Totals.Requests != 0 || idle.Currencies != nil || len(idle.Buckets) != len(snap.Buckets) {
		t.Errorf("an agent with no traffic = %+v, want zeroed buckets, no units, and the echo", idle)
	}
}

// A recognised agent is answered from its own ring, so the grouping it asked for survives the
// narrowing: Bob's models, and none of Claude's.
func TestAgentSnapshot_ARecognisedAgentKeepsItsGrouping(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	snap := twoUnitRing(t, now).AgentSnapshot(10*BucketWidth, BucketWidth, "", "bob", GroupModel)

	if snap.Group != GroupModel || snap.Agent != "bob" {
		t.Fatalf("Group = %q, Agent = %q, want model and the bob echo", snap.Group, snap.Agent)
	}
	series := FoldSeriesAcrossWindow(snap.Buckets)
	if _, ok := series["claude-opus-5"]; ok || series["premium-ide"].CostMicros != 7_800 || series["llama3"].Requests != 1 {
		t.Errorf("series = %v, want Bob's premium-ide (7800) and llama3, and no Claude model", series)
	}
}

// The agent's ring and the agent axis are two folds of one event stream, so the totals they give
// for one agent must not drift apart.
func TestAgentSnapshot_TheAgentRingAgreesWithTheAgentAxis(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := twoUnitRing(t, now)
	axis := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupAgent)
	for _, agent := range []string{"bob", "claude-code"} {
		got := a.AgentSnapshot(10*BucketWidth, BucketWidth, "", agent, GroupModel).Totals
		if want := NarrowToAgent(axis, agent).Totals; got != want {
			t.Errorf("%s: ring totals %+v, want the agent axis's %+v", agent, got, want)
		}
	}
}

// A recognised agent with no traffic yet keeps its grouping over zeroed buckets. The label, not
// whether a ring exists yet, decides the shape, so an agent's first request does not flip it.
func TestAgentSnapshot_AnIdleRecognisedAgentKeepsItsGrouping(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	snap := New(WithClock(func() time.Time { return now })).
		AgentSnapshot(10*BucketWidth, BucketWidth, "", "claude-code", GroupModel)
	if snap.Group != GroupModel || snap.Agent != "claude-code" || snap.Totals.Requests != 0 || len(snap.Buckets) != 10 {
		t.Errorf("idle claude-code = group %q agent %q requests %d over %d buckets, want model, the echo, 0, 10",
			snap.Group, snap.Agent, snap.Totals.Requests, len(snap.Buckets))
	}
}

// An agent nobody recognises gets no ring of its own — its label is a request header, so a ring
// per label would let a caller grow this process — and its grouping is served as none.
func TestAgentSnapshot_AnUnrecognisedAgentIsServedAsNone(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))
	raw := &pipeline.EventClient{Raw: "Claude-User (claude-code/2.1.284; +https://support.anthropic.com/)"}
	e := withCost(t, respEvent(now, 200, time.Second, "m", 100), 0.5)
	e.Client = raw
	a.Record("s", e)
	a.Record("s", respEvent(now, 200, time.Second, "m", 100)) // no client: the reserved "unknown"

	if len(a.agents) != 0 {
		t.Errorf("agent rings = %d, want none for a raw User-Agent or for no User-Agent", len(a.agents))
	}
	snap := a.AgentSnapshot(10*BucketWidth, BucketWidth, "", raw.Label(), GroupModel)
	if snap.Group != GroupNone || snap.Totals.Requests != 1 {
		t.Errorf("raw agent = group %q requests %d, want none over its 1 request", snap.Group, snap.Totals.Requests)
	}
}

// A session scope reads that session's ring, which no agent ring can stand in for, so agent= within
// it is served as none.
func TestAgentSnapshot_UnderASessionIsServedAsNone(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	snap := twoUnitRing(t, now).AgentSnapshot(10*BucketWidth, BucketWidth, "bob-task", "bob", GroupModel)
	if snap.Group != GroupNone || snap.Totals.CostMicros != 7_800 {
		t.Errorf("bob-task scoped to bob = group %q cost %d, want none and Bob's 7800",
			snap.Group, snap.Totals.CostMicros)
	}
}

// The agent axis is read uncapped, so an agent the response cap would fold into (other) is found.
func TestAgentSnapshot_FindsAnAgentPastTheSeriesCap(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))
	for i := range MaxSeriesInResponse + 4 {
		e := withCost(t, respEvent(now, 200, time.Second, "m", 100), float64(MaxSeriesInResponse+4-i))
		e.Client = &pipeline.EventClient{Name: fmt.Sprintf("agent%02d", i), Version: "1"}
		a.Record("s", e)
	}
	const last = "agent19"
	if got := a.AgentSnapshot(10*BucketWidth, BucketWidth, "", last, GroupNone); got.Totals.Requests != 1 {
		t.Errorf("%s past the cap: requests = %d, want its 1", last, got.Totals.Requests)
	}
}
