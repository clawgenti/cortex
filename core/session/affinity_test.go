package session

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

// Adopt reaches the aggregator through an optional interface, so a signature drift on
// either side would degrade to a silent skip. This makes it a compile error instead. It
// lives here because usage cannot import session.
var _ Rekeyer = (*usage.Aggregator)(nil)

// The same for Clear: a drift would leave per-session figures standing after a clear.
var _ Clearer = (*usage.Aggregator)(nil)
var _ Deleter = (*usage.Aggregator)(nil)

func ev() pipeline.SessionEvent {
	return pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest}
}

func TestSessionForClient_ResolutionOrder(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()

	if got := s.SessionForClient(""); got != "" {
		t.Fatalf("unknown client, empty store = %q, want \"\" (fall back to ActiveSession)", got)
	}
	if got := s.SessionForClient("bob-shell"); got != "pending:bob-shell" {
		t.Fatalf("known client with no session = %q, want its pending bucket", got)
	}

	s.Claim("claude-1", "claude-code")
	s.Append("claude-1", ev())
	if got := s.SessionForClient("claude-code"); got != "claude-1" {
		t.Fatalf("claude-code = %q, want its own session claude-1", got)
	}
	if got := s.SessionForClient("bob-shell"); got != "pending:bob-shell" {
		t.Fatalf("bob-shell = %q, want its pending bucket, never claude's session", got)
	}
	if got := s.SessionForClient(""); got != "" {
		t.Fatalf("unknown client beside ONE known agent = %q, want \"\" (single-agent behaviour unchanged)", got)
	}

	s.Append("pending:bob-shell", ev())
	if got := s.SessionForClient(""); got != DefaultSessionID {
		t.Fatalf("unknown client beside two known agents = %q, want the default bucket", got)
	}
	// Recent traffic, not expiry, decides: ttl defaults to never, so one finished Bob run
	// would otherwise send every unknown client to default for the proxy's life.
	s.mu.Lock()
	s.sessions["pending:bob-shell"].UpdatedAt = time.Now().Add(-ambiguityWindow - time.Second)
	s.mu.Unlock()
	if got := s.SessionForClient(""); got != "" {
		t.Fatalf("Bob quiet past ambiguityWindow = %q, want \"\" (fall back to ActiveSession)", got)
	}
}

func TestSessionForClient_PicksTheNewestOfTheClientsSessions(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	s.Claim("a", "claude-code")
	s.Claim("b", "claude-code")
	s.Append("a", ev())
	time.Sleep(2 * time.Millisecond)
	s.Append("b", ev())
	if got := s.SessionForClient("claude-code"); got != "b" {
		t.Fatalf("got %q, want b (updated last)", got)
	}
	time.Sleep(2 * time.Millisecond)
	s.Append("a", ev())
	if got := s.SessionForClient("claude-code"); got != "a" {
		t.Fatalf("got %q, want a (updated last)", got)
	}
}

func TestSessionForClient_SkipsExpiredSessions(t *testing.T) {
	s := New(20*time.Millisecond, 0, 0)
	defer s.Close()
	s.Claim("old", "claude-code")
	s.Append("old", ev())
	time.Sleep(40 * time.Millisecond)
	if got := s.SessionForClient("claude-code"); got != "pending:claude-code" {
		t.Fatalf("got %q, want the pending bucket once the only session expired", got)
	}
}

func TestClaim_FirstClaimWins(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	s.Claim("x", "claude-code")
	s.Claim("x", "bob-shell")
	s.Append("x", ev())
	if got := s.SessionForClient("bob-shell"); got != "pending:bob-shell" {
		t.Fatalf("bob-shell = %q; quoting claude's session id must not make it bob's", got)
	}
	if got := s.SessionForClient("claude-code"); got != "x" {
		t.Fatalf("claude-code = %q, want x", got)
	}
}

func TestClaim_AdoptsThePendingBucketAndRedirectsItsStragglers(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	s.Append("pending:bob-shell", ev())
	s.Append("pending:bob-shell", ev())

	s.Claim("task-1", "bob-shell")

	if v := s.View("pending:bob-shell"); v != nil {
		t.Fatalf("pending bucket still listed with %d events after its client's first header", len(v.Events))
	}
	v := s.View("task-1")
	if v == nil || len(v.Events) != 2 {
		t.Fatalf("task-1 = %v, want the two adopted events", v)
	}
	for _, e := range v.Events {
		if e.SessionID != "task-1" {
			t.Fatalf("adopted event still stamped %q", e.SessionID)
		}
	}
	// A response pinned to the pending id before the adoption follows it.
	s.Append("pending:bob-shell", ev())
	if v := s.View("pending:bob-shell"); v != nil {
		t.Fatal("a straggler recreated the adopted pending bucket")
	}
	if v := s.View("task-1"); len(v.Events) != 3 {
		t.Fatalf("task-1 holds %d events, want 3 with the straggler", len(v.Events))
	}
}

func TestClaim_DoesNotAdoptIntoAnotherAgentsSession(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	s.Claim("x", "claude-code")
	s.Append("pending:bob-shell", ev())
	s.Claim("x", "bob-shell")
	if v := s.View("pending:bob-shell"); v == nil {
		t.Fatal("bob's pending bucket was merged into claude's session")
	}
}

func TestAdopt_LeavesPendingWhenTheTargetHoldsEvents(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	s.Append("pending:bob-shell", ev())
	s.Append("task-1", ev())
	if s.Adopt("pending:bob-shell", "task-1") {
		t.Fatal("Adopt merged into a session that already held events")
	}
	if s.View("pending:bob-shell") == nil || len(s.View("task-1").Events) != 1 {
		t.Fatal("a refused Adopt changed the store")
	}
}

type rekeyRecorder struct{ rekeyed [][2]string }

func (r *rekeyRecorder) Record(string, *pipeline.SessionEvent) {}
func (r *rekeyRecorder) Rekeyed(o, n string)                   { r.rekeyed = append(r.rekeyed, [2]string{o, n}) }

// Every rename the store makes is announced to the recorders keeping per-session state, the
// A2A merge included: a Recorder that misses one keeps figures, or history, under an id
// nothing is filed under any more.
func TestRekeyAndAdopt_BothNotifyRekeyers(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	r := &rekeyRecorder{}
	s.AddRecorder(r)

	s.Append(DefaultSessionID, ev())
	s.Rekey(DefaultSessionID, "ctx-1")
	s.Append("pending:bob-shell", ev())
	s.Claim("task-1", "bob-shell")

	want := [][2]string{{DefaultSessionID, "ctx-1"}, {"pending:bob-shell", "task-1"}}
	if !slices.Equal(r.rekeyed, want) {
		t.Fatalf("Rekeyed calls = %v, want %v", r.rekeyed, want)
	}
}

// A refused rename changed nothing, so it must announce nothing.
func TestRekey_ARefusedRenameNotifiesNoOne(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	r := &rekeyRecorder{}
	s.AddRecorder(r)

	s.Append(DefaultSessionID, ev())
	s.Append("ctx-1", ev())
	s.Rekey(DefaultSessionID, "ctx-1") // target exists: refused
	s.Rekey("absent", "ctx-2")         // source absent: refused

	if len(r.rekeyed) != 0 {
		t.Fatalf("Rekeyed calls = %v, want none", r.rekeyed)
	}
}

func tokensEv(in int) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At: time.Now(), Phase: pipeline.SessionResponse, StatusCode: 200, Host: "gw.example.com",
		Inference: &pipeline.InferenceExtension{
			Model: "m", InputTokens: in, OutputTokens: 1, TotalTokens: in + 1, PresentKinds: 0b1001,
		},
	}
}

// The store evicts a session without telling the aggregator, whose ring for it lives on, and
// then permits a rename into that id. /v1/usage must still agree with itself afterwards: the
// session's own figure is its row in the all-sessions breakdown, and the old id answers nothing.
func TestRename_IntoAnEvictedSessionKeepsUsageConsistent(t *testing.T) {
	for _, c := range []struct {
		name, from string
		rename     func(s *Store, from string)
	}{
		{"Rekey", DefaultSessionID, func(s *Store, from string) { s.Rekey(from, "ctx-1") }},
		{"Adopt", PendingSessionID("claude-code"), func(s *Store, _ string) { s.Claim("ctx-1", "claude-code") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			agg := usage.New()
			s := New(0, 0, 2)
			defer s.Close()
			s.AddRecorder(agg)

			s.Append("ctx-1", tokensEv(13))
			s.Append("x1", tokensEv(1))
			s.Append("x2", tokensEv(1)) // evicts ctx-1 from the store
			s.Append(c.from, tokensEv(17))
			c.rename(s, c.from)

			if s.View("ctx-1") == nil || s.View(c.from) != nil {
				t.Fatal("the rename did not happen, so this test checks nothing")
			}
			window := 10 * time.Minute
			var row int64
			for _, b := range agg.Snapshot(window, usage.BucketWidth, "", usage.GroupSession).Buckets {
				row += b.Series["ctx-1"].InputTokens
			}
			one := agg.Snapshot(window, usage.BucketWidth, "ctx-1", usage.GroupNone).Totals.InputTokens
			if one != 30 || row != 30 {
				t.Errorf("session=ctx-1 InputTokens = %d, all-sessions row = %d, want 30 for both", one, row)
			}
			if got := agg.Snapshot(window, usage.BucketWidth, c.from, usage.GroupNone).Totals.InputTokens; got != 0 {
				t.Errorf("session=%s InputTokens = %d, want 0 once renamed", c.from, got)
			}
		})
	}
}

// Adoption into a session that already holds events cannot succeed, so Claim tries it on
// the session's first claim and not on every headered request after, each of which would
// log that the bucket was not adopted.
func TestClaim_DoesNotRetryAnAdoptionThatCannotSucceed(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	s := New(0, 0, 0)
	defer s.Close()
	s.Append("resumed", ev())
	s.Append(PendingSessionID("claude-code"), ev())
	for range 5 {
		s.Claim("resumed", "claude-code")
	}
	if n := strings.Count(logs.String(), "pending bucket not adopted"); n != 1 {
		t.Errorf("%d \"not adopted\" lines for 5 claims, want 1", n)
	}
}
