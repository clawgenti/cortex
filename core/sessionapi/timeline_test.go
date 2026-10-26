package sessionapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/timeline"
)

func getTimeline(t *testing.T, base, id, query string) (*timeline.Timeline, int) {
	t.Helper()
	resp, err := http.Get(base + "/v1/sessions/" + url.PathEscape(id) + "/timeline" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
	var tl timeline.Timeline
	if err := json.NewDecoder(resp.Body).Decode(&tl); err != nil {
		t.Fatal(err)
	}
	return &tl, resp.StatusCode
}

func appendTurn(store *session.Store, id string, at time.Time, rid, toolID string) {
	store.Append(id, pipeline.SessionEvent{At: at, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, RequestID: rid, Host: "llm.example",
		Inference: &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleMain, Tools: []pipeline.InferenceTool{{Name: "Bash"}}}})
	store.Append(id, pipeline.SessionEvent{At: at.Add(time.Second), Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, RequestID: rid, Host: "llm.example",
		StatusCode: 200, Duration: time.Second,
		Inference: &pipeline.InferenceExtension{FinishReason: "tool_use", ToolCalls: []pipeline.InferenceToolCall{{ID: toolID, Name: "Bash"}}}})
}

func TestTimelineServesSpansAndClips(t *testing.T) {
	ts, store := newTestServer(t)
	base := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	appendTurn(store, "s1", base, "r1", "t1")
	appendTurn(store, "s1", base.Add(time.Hour), "r2", "t2")

	tl, code := getTimeline(t, ts.URL, "s1", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(tl.Turns) != 2 || len(tl.Tools) != 2 || len(tl.Hosts) != 2 || tl.LastSeq != 4 || tl.Truncated {
		t.Errorf("timeline = %+v", tl)
	}

	from := url.QueryEscape(base.Add(30 * time.Minute).Format(time.RFC3339))
	tl, _ = getTimeline(t, ts.URL, "s1", "?from="+from)
	// t1 was never answered but its agent moved on, so it is lost and ends where it started;
	// t2 is still in flight, which overlaps every later window.
	if len(tl.Turns) != 1 || len(tl.Tools) != 1 || tl.Tools[0].ID != "t2" || len(tl.Agents) != 1 {
		t.Errorf("clipped = %+v, want only the second turn and tool, and the agent kept", tl)
	}
}

func TestTimelineUnknownSessionAndBadTime(t *testing.T) {
	ts, store := newTestServer(t)
	if _, code := getTimeline(t, ts.URL, "nope", ""); code != http.StatusNotFound {
		t.Errorf("unknown session: status %d, want 404", code)
	}
	appendTurn(store, "s1", time.Now(), "r1", "t1")
	if _, code := getTimeline(t, ts.URL, "s1", "?to=yesterday"); code != http.StatusBadRequest {
		t.Errorf("bad time: status %d, want 400", code)
	}
}

// After a restart the timeline covers the whole session: the archive's history behind the
// few events the new process holds.
func TestTimelineReadsTheArchiveBehindMemory(t *testing.T) {
	ts, store, _ := restarted(t)
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, RequestID: "new", Host: "after.example"})

	tl, code := getTimeline(t, ts.URL, "s1", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if tl.LastSeq != 11 {
		t.Errorf("lastSeq = %d, want 11 (10 archived + 1 resident)", tl.LastSeq)
	}
	// The archived events in restarted carry no direction, so only the resident one is a host.
	if len(tl.Hosts) != 1 || tl.Hosts[0].Host != "after.example" {
		t.Errorf("hosts = %+v", tl.Hosts)
	}
	// A session only the archive holds is served, not 404'd.
	if _, code := getTimeline(t, ts.URL, "old", ""); code != http.StatusOK {
		t.Errorf("archived-only session: status %d, want 200", code)
	}
}

// A cached timeline is served until the session gains an event, and a caller's clip does
// not reach the cached copy.
func TestTimelineCacheFollowsTheNewestEvent(t *testing.T) {
	ts, store := newTestServer(t)
	base := time.Now().Add(-time.Hour)
	appendTurn(store, "s1", base, "r1", "t1")
	appendTurn(store, "s1", base.Add(30*time.Minute), "r2", "t2")

	from := url.QueryEscape(base.Add(20 * time.Minute).Format(time.RFC3339))
	getTimeline(t, ts.URL, "s1", "") // fill the cache, so the clipped read below is a hit
	if tl, _ := getTimeline(t, ts.URL, "s1", "?from="+from); len(tl.Turns) != 1 {
		t.Fatalf("clipped turns = %d, want 1", len(tl.Turns))
	}
	if tl, _ := getTimeline(t, ts.URL, "s1", ""); len(tl.Turns) != 2 {
		t.Errorf("after a clipped read, unclipped turns = %d, want 2: the clip reached the cache", len(tl.Turns))
	}
	appendTurn(store, "s1", base.Add(40*time.Minute), "r3", "t3")
	if tl, _ := getTimeline(t, ts.URL, "s1", ""); len(tl.Turns) != 3 || tl.LastSeq != 6 {
		t.Errorf("after a new event: turns = %d lastSeq = %d, want 3 and 6", len(tl.Turns), tl.LastSeq)
	}
}

// A session past the read bound says so.
func TestTimelineSaysWhenItIsTruncated(t *testing.T) {
	store := session.New(0, 0, 0) // uncapped: newTestServer's store holds only 100 events
	ts := httptest.NewServer(New(":0", store).server.Handler)
	t.Cleanup(func() { ts.Close(); store.Close() })
	at := time.Now()
	for i := 0; i < timelineMaxEvents+5; i++ {
		store.Append("big", pipeline.SessionEvent{At: at, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "h.example"})
	}
	store.Append("small", pipeline.SessionEvent{At: at, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "h.example"})
	if tl, _ := getTimeline(t, ts.URL, "big", ""); !tl.Truncated || tl.LastSeq != timelineMaxEvents+5 {
		t.Errorf("big: truncated = %v lastSeq = %d", tl.Truncated, tl.LastSeq)
	}
	if tl, _ := getTimeline(t, ts.URL, "small", ""); tl.Truncated {
		t.Errorf("small session reported truncated")
	}
}
