package sessionapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

func doClear(t *testing.T, base string, edit func(*http.Request)) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, base+"/v1/sessions", nil)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

type clearBody struct {
	Sessions         int    `json:"sessions"`
	ArchivedSessions int    `json:"archivedSessions"`
	Bytes            int64  `json:"bytes"`
	ArchiveError     string `json:"archiveError"`
	Error            string `json:"error"`
}

func decodeClear(t *testing.T, b []byte) clearBody {
	t.Helper()
	var c clearBody
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return c
}

// The first state-changing endpoint on an unauthenticated API, so each guard is pinned on its
// own. What they defend against is a browser: a page the user visits that targets the loopback
// port directly, or through a DNS name rebound to 127.0.0.1.
func TestHandleClear_Guards(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed bool
		method  string
		edit    func(*http.Request)
		want    int
	}{
		{"loopback, no origin", true, http.MethodDelete, nil, http.StatusOK},
		{"localhost without a port", true, http.MethodDelete, func(r *http.Request) { r.Host = "localhost" }, http.StatusOK},
		{"localhost with a port", true, http.MethodDelete, func(r *http.Request) { r.Host = "localhost:9094" }, http.StatusOK},
		{"ipv6 loopback", true, http.MethodDelete, func(r *http.Request) { r.Host = "[::1]:9094" }, http.StatusOK},
		{"a rebound name", true, http.MethodDelete, func(r *http.Request) { r.Host = "evil.example:9094" }, http.StatusForbidden},
		{"a name that starts like localhost", true, http.MethodDelete, func(r *http.Request) { r.Host = "localhost.evil.example" }, http.StatusForbidden},
		{"another loopback address", true, http.MethodDelete, func(r *http.Request) { r.Host = "127.0.0.2:9094" }, http.StatusForbidden},
		{"a browser's origin", true, http.MethodDelete, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, http.StatusForbidden},
		{"even a null origin", true, http.MethodDelete, func(r *http.Request) { r.Header.Set("Origin", "null") }, http.StatusForbidden},
		{"not bound to loopback only", false, http.MethodDelete, nil, http.StatusForbidden},
		{"POST, which needs no preflight", true, http.MethodPost, nil, http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, store := newTestServer(t, WithClearAllowed(tc.allowed))
			store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
			req, _ := http.NewRequest(tc.method, ts.URL+"/v1/sessions", nil)
			if tc.edit != nil {
				tc.edit(req)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("%s = %d %s, want %d", tc.method, resp.StatusCode, body, tc.want)
			}
			cleared := len(store.ListSessions()) == 0
			if cleared != (tc.want == http.StatusOK) {
				t.Fatalf("status %d but cleared=%v", resp.StatusCode, cleared)
			}
			if tc.want == http.StatusForbidden && decodeClear(t, body).Error == "" {
				t.Fatalf("a refusal without a reason: %s", body)
			}
		})
	}
	ts, _ := newTestServer(t, WithClearAllowed(true))
	if code, _ := getBody(t, ts.URL+"/v1/sessions"); code != http.StatusOK {
		t.Fatalf("GET /v1/sessions = %d beside the DELETE route", code)
	}
}

// The response says what went: sessions from memory, sessions and bytes from disk. Afterwards
// nothing is listed, with or without ?archived=true.
func TestHandleClear_ReportsCounts(t *testing.T) {
	ts, store, _ := restarted(t, WithClearAllowed(true))
	store.Append("live", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	code, b := doClear(t, ts.URL, nil)
	if code != http.StatusOK {
		t.Fatalf("DELETE = %d %s", code, b)
	}
	c := decodeClear(t, b)
	// Three on disk: s1 and old from before the restart, and live, which the archive recorded too.
	if c.Sessions != 1 || c.ArchivedSessions != 3 || c.Bytes <= 0 || c.ArchiveError != "" {
		t.Fatalf("clear = %+v, want 1 in memory, 3 on disk with their bytes", c)
	}
	if lb := getList(t, ts.URL+"/v1/sessions?archived=true"); len(lb.Sessions) != 0 {
		t.Fatalf("listed after a clear: %+v", lb.Sessions)
	}
	if code, _ := getBody(t, ts.URL+"/v1/sessions/s1"); code != http.StatusNotFound {
		t.Fatalf("GET an archived session after a clear = %d, want 404", code)
	}
}

// Memory is cleared whatever the disk does — it already was, under the store's lock — so a
// failure on disk is a 500 that still reports the memory half, plus why the disk half failed.
func TestHandleClear_ArchiveErrorIs500WithMemoryCounts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permission this test relies on")
	}
	ts, store, a := restarted(t, WithClearAllowed(true))
	store.Append("live", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	if err := os.Chmod(a.Root(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(a.Root(), 0o700) })
	code, b := doClear(t, ts.URL, nil)
	c := decodeClear(t, b)
	if code != http.StatusInternalServerError || c.Sessions != 1 || c.ArchiveError == "" {
		t.Fatalf("DELETE = %d %+v, want 500 with the memory count and an archiveError", code, c)
	}
	if len(store.ListSessions()) != 0 {
		t.Fatal("memory was not cleared")
	}
}

// The cost ledger holds spend totals and no content, and a clear leaves it alone: today's spend
// reads the same before and after.
func TestHandleClear_LeavesTheCostLedgerUntouched(t *testing.T) {
	now := time.Now()
	led := newTestLedger(t, now)
	agg := usage.New()
	ts, store := newTestServer(t, WithClearAllowed(true), WithUsage(agg), WithCostLedger(led))
	store.AddRecorder(agg)
	store.AddRecorder(led)
	store.Append("s1", *agentResponse(t, now, &pipeline.EventClient{Name: "claude-code", Version: "2.1"}, 0.25))
	if err := led.Flush(); err != nil {
		t.Fatal(err)
	}
	spend := func() int64 {
		_, body := fetchUsage(t, ts.URL, "?window=today")
		var snap usage.Snapshot
		if err := json.Unmarshal([]byte(body), &snap); err != nil {
			t.Fatal(err)
		}
		return snap.Totals.CostMicros
	}
	before := spend()
	if before != 250_000 {
		t.Fatalf("today's spend before the clear = %d, want 250000", before)
	}
	if code, b := doClear(t, ts.URL, nil); code != http.StatusOK {
		t.Fatalf("DELETE = %d %s", code, b)
	}
	if after := spend(); after != before {
		t.Fatalf("today's spend after the clear = %d, want %d", after, before)
	}
}

// A refusal is logged, but not once per request: a rebinding page can loop this endpoint, and a
// WARN each would grow the proxy's log without bound.
func TestHandleClear_RefusalsAreLoggedOncePerInterval(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ts, _ := newTestServer(t, WithClearAllowed(true))
	for range 20 {
		if code, _ := doClear(t, ts.URL, func(r *http.Request) { r.Host = "evil.example" }); code != http.StatusForbidden {
			t.Fatalf("DELETE = %d, want 403", code)
		}
	}
	if n := strings.Count(logs.String(), "refused to erase"); n != 1 {
		t.Fatalf("20 refusals logged %d lines, want 1:\n%s", n, logs.String())
	}
}
