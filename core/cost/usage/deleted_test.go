package usage

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// Deleted drops every figure answerable by the erased id — its own ring and its row in the
// all-sessions and per-agent breakdowns — and leaves the other sessions and every total alone.
func TestDeleted_DropsOneSessionsFiguresAndKeepsTotals(t *testing.T) {
	a := New()
	e := inferenceEvent("m", 13, 0, 0, 5, 0, 0b1001)
	e.Client = &pipeline.EventClient{Name: "claude-code", Version: "1.0"}
	a.Record("s1", e)
	a.Record("s2", inferenceEvent("m", 29, 0, 0, 7, 0, 0b1001))

	a.Deleted("s1")

	if got := a.Snapshot(10*time.Minute, BucketWidth, "", GroupNone).Totals.InputTokens; got != 42 {
		t.Errorf("all-sessions InputTokens = %d, want 42 kept", got)
	}
	if got := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupNone).Totals.InputTokens; got != 0 {
		t.Errorf("session=s1 InputTokens = %d, want 0 after its delete", got)
	}
	if got := a.Snapshot(10*time.Minute, BucketWidth, "s2", GroupNone).Totals.InputTokens; got != 29 {
		t.Errorf("session=s2 InputTokens = %d, want 29 untouched", got)
	}
	all := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "", GroupSession).Buckets)
	if _, ok := all["s1"]; ok || len(all) != 1 {
		t.Errorf("all-sessions breakdown = %v, want s2 alone", all)
	}
	if s := mergeSeries(a.AgentSnapshot(10*time.Minute, BucketWidth, "", "claude-code", GroupSession).Buckets); len(s) != 0 {
		t.Errorf("claude-code's ring still names s1: %v", s)
	}
	if got := a.AgentSnapshot(10*time.Minute, BucketWidth, "", "claude-code", GroupNone).Totals.InputTokens; got != 13 {
		t.Errorf("claude-code's total = %d, want 13 kept", got)
	}
}
