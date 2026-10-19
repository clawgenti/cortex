package sessionapi

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDocCoversEveryRegisteredRoute pins that session-api.md — positioned as
// the complete session-API reference — lists every route server.go registers.
// DELETE /v1/sessions/{id} shipped in #1422 after the doc was written, and
// GET /v1/usage was never listed; both were found by review (PR #1423).
func TestDocCoversEveryRegisteredRoute(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("server.go"))
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "session-api.md"))
	if err != nil {
		t.Fatalf("read docs/session-api.md: %v", err)
	}
	for _, m := range regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+) ([^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		route := m[1] + " " + m[2]
		// Go 1.22 pattern for the index route; the doc spells it plainly.
		route = strings.Replace(route, "/{$}", "/", 1)
		if !strings.Contains(string(doc), "`"+route+"`") {
			t.Errorf("session-api.md does not list %s, which server.go registers", route)
		}
	}
}
