package sessionapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// TestDocExamplePromptContextMatchesWire pins that the promptContext value in
// session-api.md's GET /v1/sessions example unmarshals into the type the
// server actually sends. The example showed a bare number where the wire
// carries an object {tokens, stated, at, msgs} (PR #1423 review).
func TestDocExamplePromptContextMatchesWire(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "session-api.md"))
	if err != nil {
		t.Fatalf("read docs/session-api.md: %v", err)
	}
	// Anchor to the GET /v1/sessions section so a promptContext value elsewhere
	// in the doc cannot silently re-target this test.
	doc := string(raw)
	sec := strings.Index(doc, "## `GET /v1/sessions`")
	if sec < 0 {
		t.Fatal("session-api.md has no GET /v1/sessions section")
	}
	scope := doc[sec:]
	if next := strings.Index(scope[3:], "\n## "); next >= 0 {
		scope = scope[:next+3]
	}
	m := regexp.MustCompile(`"promptContext": (\{[^}]*\}|-?\d+)`).FindSubmatch([]byte(scope))
	if m == nil {
		t.Fatal("session-api.md's GET /v1/sessions example carries no promptContext value")
	}
	var pc pipeline.PromptContext
	if err := json.Unmarshal(m[1], &pc); err != nil {
		t.Errorf("the example's promptContext %s does not match the wire type pipeline.PromptContext: %v", m[1], err)
	}
	if pc.Tokens == 0 {
		t.Errorf("the example's promptContext %s has tokens = 0; an example should carry a real figure", m[1])
	}
}
