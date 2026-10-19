package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The revision `make agentop` stamps is the revision of the tree being built, which for a
// checkout under .worktrees/ is not the enclosing checkout's. Go's own VCS stamp gets that
// wrong: it recognises only a .git DIRECTORY, so in a worktree, whose .git is a file, it walks
// up and records the outer checkout's HEAD and dirty state instead.
func TestMakefileStampsTheBuiltTreesRevision(t *testing.T) {
	for _, tool := range []string{"git", "make"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	makefile, err := filepath.Abs(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}

	outer := t.TempDir()
	git(t, outer, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(outer, "a.txt"), "outer\n")
	git(t, outer, "add", "a.txt")
	git(t, outer, "commit", "-q", "-m", "outer")

	wt := filepath.Join(outer, ".worktrees", "topic")
	git(t, outer, "worktree", "add", "-q", "-b", "topic", wt)
	writeFile(t, filepath.Join(wt, "b.txt"), "topic\n")
	git(t, wt, "add", "b.txt")
	git(t, wt, "commit", "-q", "-m", "topic")
	want := git(t, wt, "rev-parse", "--short=8", "HEAD")
	if outerRev := git(t, outer, "rev-parse", "--short=8", "HEAD"); outerRev == want {
		t.Fatalf("fixture: outer and worktree share HEAD %s", want)
	}

	// The outer checkout is dirty here (.worktrees/ is untracked in it) and the worktree is
	// clean, which is the layout that produced a false "+dirty".
	if got := stampedRevision(t, makefile, wt); got != want {
		t.Errorf("clean worktree: stamped %q, want %q", got, want)
	}

	writeFile(t, filepath.Join(wt, "untracked.txt"), "x\n")
	if got := stampedRevision(t, makefile, wt); got != want+"+dirty" {
		t.Errorf("dirty worktree: stamped %q, want %q", got, want+"+dirty")
	}

	// Outside any checkout there is no revision to name.
	if got := stampedRevision(t, makefile, t.TempDir()); got != "" {
		t.Errorf("no checkout: stamped %q, want none", got)
	}
}

func TestDisplayVersionOf(t *testing.T) {
	for _, tc := range []struct {
		version, revision, want string
	}{
		{"v0.8.0", "f256b3c2", "v0.8.0"},
		{"dev", "", "dev"},
		{"dev", "f256b3c2", "dev-f256b3c2"},
		{"dev", "f256b3c2+dirty", "dev-f256b3c2+dirty"},
	} {
		if got := displayVersionOf(tc.version, tc.revision); got != tc.want {
			t.Errorf("displayVersionOf(%q, %q) = %q, want %q", tc.version, tc.revision, got, tc.want)
		}
	}
}

var revisionFlag = regexp.MustCompile(`-X main\.revision=(\S*)`)

// stampedRevision is the main.revision the Makefile's agentop recipe would pass when run in
// dir, read from a dry run so nothing is built.
func stampedRevision(t *testing.T, makefile, dir string) string {
	t.Helper()
	out, err := exec.Command("make", "-n", "-s", "-C", dir, "-f", makefile, "agentop").CombinedOutput()
	if err != nil {
		t.Fatalf("make -n agentop in %s: %v\n%s", dir, err, out)
	}
	m := revisionFlag.FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("make -n agentop in %s passes no -X main.revision:\n%s", dir, out)
	}
	return strings.Trim(m[1], `"'`)
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
