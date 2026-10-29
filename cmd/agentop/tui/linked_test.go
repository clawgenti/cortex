package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// A pending bucket every request of which is linked to a session is not a row of its own;
// one with calls after its process's last claim still is.
func TestSessionsPane_HidesAFullyLinkedBucket(t *testing.T) {
	const all, part = "pending:claude-code@1.1", "pending:claude-code@2.2"
	m := uuidPicker(t, 200, uuidOpened, all, part)
	until := time.Now()
	for i := range m.sessions {
		switch m.sessions[i].ID {
		case all:
			m.sessions[i].LinkedTo, m.sessions[i].LinkedAll = []session.PendingLink{{Session: uuidOpened, First: until, Last: until}}, true
		case part:
			m.sessions[i].LinkedTo = []session.PendingLink{{Session: uuidOpened, First: until, Last: until}}
		}
	}
	m.rebuildSessionsTable()
	if !slices.Equal(m.sessionRowIDs, []string{uuidOpened, part}) {
		t.Errorf("rows = %v, want the session and the partly linked bucket only", m.sessionRowIDs)
	}
}

// Opening a session merges its linked calls into the timeline at the time they happened —
// not on top: for a resumed session they belong days after its first rows. Each keeps the
// bucket as its SessionID, which is where the detail pane fetches it from.
func TestSnapshot_MergesLinkedCallsByTime(t *testing.T) {
	const bucket = "pending:claude-code@1.1"
	t0 := time.Unix(1_700_000_000, 0).UTC()
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	views := map[string]pipeline.SessionView{
		"s1": {ID: "s1", Events: []pipeline.SessionEvent{
			{SessionID: "s1", Seq: 1, At: at(0), Phase: pipeline.SessionRequest, RequestID: "yesterday"},
			{SessionID: "s1", Seq: 2, At: at(100), Phase: pipeline.SessionRequest, RequestID: "today"},
		}},
		bucket: {ID: bucket, Events: []pipeline.SessionEvent{
			{SessionID: bucket, Seq: 1, At: at(50), Phase: pipeline.SessionRequest, RequestID: "mcp-init"},
			{SessionID: bucket, Seq: 2, At: at(51), Phase: pipeline.SessionResponse, RequestID: "mcp-init"},
			{SessionID: bucket, Seq: 3, At: at(200), Phase: pipeline.SessionRequest, RequestID: "after-claim"},
		}},
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
		v, ok := views[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
	defer ts.Close()

	m := &model{ctx: context.Background(), client: apiclient.New(ts.URL), sessions: []session.SessionSummary{
		{ID: "s1"},
		{ID: bucket, LinkedTo: []session.PendingLink{{Session: "s1", First: at(100), Last: at(100)}}},
	}}
	msg := m.snapshotCmd("s1")().(snapshotLoadedMsg)
	var got []string
	for _, e := range msg.events {
		got = append(got, e.SessionID+"/"+e.RequestID)
	}
	want := []string{"s1/yesterday", bucket + "/mcp-init", bucket + "/mcp-init", "s1/today"}
	if !slices.Equal(got, want) {
		t.Errorf("timeline = %v, want %v", got, want)
	}
	if got := eventSession("s1", &msg.events[1]); got != bucket {
		t.Errorf("a linked event's detail is fetched from %q, want %q", got, bucket)
	}
}

// A session longer than one snapshot still shows the calls a resume linked into its newest
// page; one older than the page is left to the page that holds its time.
func TestInWindow_KeepsTheLinkedCallsTheSnapshotCovers(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	view := &pipeline.SessionView{TotalEvents: 5000, Events: []pipeline.SessionEvent{{At: t0.Add(time.Hour)}}}
	linked := []pipeline.SessionEvent{{RequestID: "old", At: t0}, {RequestID: "resume", At: t0.Add(2 * time.Hour)}}
	if got := ids(inWindow(linked, view)); !slices.Equal(got, []string{"resume"}) {
		t.Errorf("inWindow = %v, want only the call inside the page", got)
	}
	whole := &pipeline.SessionView{Events: view.Events}
	if got := ids(inWindow([]pipeline.SessionEvent{{RequestID: "old", At: t0}}, whole)); !slices.Equal(got, []string{"old"}) {
		t.Errorf("a whole session = %v, want every linked call", got)
	}
}

func ids(events []pipeline.SessionEvent) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.RequestID)
	}
	return out
}

// A bucket is folded only when it belongs to exactly one session: one linked to two (a /clear
// in its middle) stays a row, because its calls and its spend cannot be given to one row.
func TestFoldedInto(t *testing.T) {
	one := []session.PendingLink{{Session: "s1"}}
	two := []session.PendingLink{{Session: "s1"}, {Session: "s2"}}
	for _, tc := range []struct {
		s    session.SessionSummary
		want string
	}{
		{session.SessionSummary{ID: "pending:a@1.1", LinkedTo: one, LinkedAll: true}, "s1"},
		{session.SessionSummary{ID: "pending:a@1.1", LinkedTo: one}, ""},
		{session.SessionSummary{ID: "pending:a@1.1", LinkedTo: two, LinkedAll: true}, ""},
	} {
		if got := foldedInto(tc.s); got != tc.want {
			t.Errorf("foldedInto(%+v) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// A folded bucket's spend and tokens are its session's row's: hidden, they would show nowhere.
func TestSessionsPane_AFoldedBucketsFiguresJoinItsSession(t *testing.T) {
	const bucket = "pending:bob@1.1"
	m := uuidPicker(t, 200, uuidOpened, bucket)
	for i := range m.sessions {
		m.sessions[i].CostMicros, m.sessions[i].TotalTokens = 1_000_000, 100
		if m.sessions[i].ID == bucket {
			m.sessions[i].LinkedTo, m.sessions[i].LinkedAll = []session.PendingLink{{Session: uuidOpened}}, true
		}
	}
	m.rebuildSessionsTable()
	row := m.sessionsTbl.Rows()[rowOf(t, m, uuidOpened)]
	if !slices.ContainsFunc(row, func(c string) bool { return strings.Contains(c, "$2.00") }) {
		t.Errorf("session row %v, want the bucket's $1.00 added to its own", row)
	}
}

// Paging back into a session merges the folded calls that fall in the older page, so a fresh
// launch's handshake is reachable in a session longer than one snapshot.
func TestOlderPage_MergesTheLinkedCallsItCovers(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	page := []pipeline.SessionEvent{{SessionID: "s1", RequestID: "p1", At: at(10)}, {SessionID: "s1", RequestID: "p2", At: at(20)}}
	linked := []pipeline.SessionEvent{{SessionID: "b", RequestID: "init", At: at(5)}, {SessionID: "b", RequestID: "later", At: at(500)}}
	got := ids(pageLinked(page, linked, true, at(30)))
	if !slices.Equal(got, []string{"init", "p1", "p2"}) {
		t.Errorf("oldest page = %v, want the handshake before it merged", got)
	}
	got = ids(pageLinked(page, linked, false, at(30)))
	if !slices.Equal(got, []string{"p1", "p2"}) {
		t.Errorf("a middle page = %v, want none: the later call is at or after the held window", got)
	}
}

// A linked call between an older page's last event and the oldest event already held goes on
// that page: the held window takes calls from its own oldest event on, so nothing else shows it.
func TestOlderPage_TakesTheLinkedCallsUpToTheHeldWindow(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	page := []pipeline.SessionEvent{{SessionID: "s1", RequestID: "p1", At: at(90)}, {SessionID: "s1", RequestID: "p2", At: at(100)}}
	linked := []pipeline.SessionEvent{
		{SessionID: "b", RequestID: "gap", At: at(150)},
		{SessionID: "b", RequestID: "held", At: at(200)},
	}
	if got := ids(pageLinked(page, linked, false, at(200))); !slices.Equal(got, []string{"p1", "p2", "gap"}) {
		t.Errorf("older page = %v, want the call between it and the held window", got)
	}
	tail := &pipeline.SessionView{Events: []pipeline.SessionEvent{{SessionID: "s1", At: at(200)}}, TotalEvents: 3}
	if got := ids(inWindow(slices.Clone(linked), tail)); !slices.Equal(got, []string{"held"}) {
		t.Errorf("tail window = %v, want only the call at its first event", got)
	}
}

// Erasing a session erases the buckets folded into it: the calls the operator saw inside it
// would otherwise come back as a row of their own.
func TestEraseTargets_IncludeTheBucketsFoldedIntoTheSession(t *testing.T) {
	m := &model{sessions: []session.SessionSummary{
		{ID: "s1"},
		{ID: "pending:a@1.1", LinkedTo: []session.PendingLink{{Session: "s1"}}, LinkedAll: true},
		{ID: "pending:a@2.2", LinkedTo: []session.PendingLink{{Session: "s1"}, {Session: "s2"}}, LinkedAll: true},
	}}
	if got := m.withFolded([]string{"s1"}); !slices.Equal(got, []string{"s1", "pending:a@1.1"}) {
		t.Errorf("withFolded = %v, want s1 and only the bucket folded into it alone", got)
	}
}
