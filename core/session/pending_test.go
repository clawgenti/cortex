package session

import "testing"

// PendingID is the bucket id the recorder files a not-yet-named process's calls under. Its
// format is the one the archive already holds, so rows written before this change still read.
func TestPendingID(t *testing.T) {
	for _, tc := range []struct{ agent, proc, want string }{
		{"claude-code", "100.5", "pending:claude-code@100.5"},
		{"bob", "", "pending:bob"},
	} {
		if got := PendingID(tc.agent, tc.proc); got != tc.want {
			t.Errorf("PendingID(%q, %q) = %q, want %q", tc.agent, tc.proc, got, tc.want)
		}
	}
	if got, want := PendingID("claude-code", "100.5"), PendingProcessID("claude-code", Proc{PID: 100, Start: 5}); got != want {
		t.Errorf("PendingID = %q, PendingProcessID = %q: the two formats must agree until Task 3 removes the second", got, want)
	}
}
