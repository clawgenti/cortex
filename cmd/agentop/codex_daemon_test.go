package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const cortexProxyURL = "http://127.0.0.1:47600" // the forward proxy in cortexCfg

// fakeCodexDaemon is the daemon a test sees: a pid file under its CODEX_HOME, a process
// table that says that pid is alive, and an environment for it. restart, when not
// failing, replaces it with newPID running with newEnv, as Codex's restart does.
type fakeCodexDaemon struct {
	pid    int
	env    []string
	envErr error

	newPID     int
	newEnv     []string
	newGone    bool  // the restarted daemon exits straight away
	restartErr error // the restart command fails

	restarts []map[string]string // the want each restart was given
}

// stubCodexDaemon points every daemon seam at f for one test, so nothing a test does can
// restart a real daemon or read a real process: the pid is fake, and a real process with
// that pid would otherwise be the one inspected.
func stubCodexDaemon(t *testing.T, codexHome string, f *fakeCodexDaemon) {
	t.Helper()
	alive, environ, restart := codexDaemonAlive, codexEnviron, codexRestartDaemon
	t.Cleanup(func() { codexDaemonAlive, codexEnviron, codexRestartDaemon = alive, environ, restart })
	if f.pid != 0 {
		writeCodexDaemonPID(t, codexHome, f.pid)
	}
	codexDaemonAlive = func(pid int) bool { return pid == f.pid }
	codexEnviron = func(pid int32) ([]string, error) {
		if int(pid) != f.pid {
			t.Errorf("read the environment of pid %d, which is not the fake daemon's", pid)
			return nil, errors.New("not the fake daemon")
		}
		return f.env, f.envErr
	}
	codexRestartDaemon = func(home string, want map[string]string) error {
		if home != codexHome {
			t.Errorf("restarted the daemon of %s, want %s", home, codexHome)
		}
		f.restarts = append(f.restarts, want)
		if f.restartErr != nil {
			return f.restartErr
		}
		f.pid, f.env, f.envErr = f.newPID, f.newEnv, nil
		if f.newGone {
			f.pid = 0
		} else {
			writeCodexDaemonPID(t, codexHome, f.newPID)
		}
		return nil
	}
}

// writeCodexDaemonPID writes a daemon.pid under codexHome as Codex does.
func writeCodexDaemonPID(t *testing.T, codexHome string, pid int) {
	t.Helper()
	dir := filepath.Join(codexHome, filepath.Dir(codexDaemonPIDRel))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"pid":` + strconv.Itoa(pid) + `,"processStartTime":"Fri Oct 23 09:03:25 2026"}`
	if err := os.WriteFile(filepath.Join(codexHome, codexDaemonPIDRel), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// onCortex is a daemon environment routed through Cortex, as enable's file makes it.
var onCortex = []string{"HOME=/home/someone", "HTTPS_PROXY=" + cortexProxyURL, "https_proxy=" + cortexProxyURL}

// offCortex is one with no proxy at all.
var offCortex = []string{"HOME=/home/someone"}

func enableCodex(t *testing.T, envPath, cfgPath string) {
	t.Helper()
	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("enable exit %d: %s", code, errb.String())
	}
}

func wantAll(t *testing.T, what, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, got)
		}
	}
}

// Enable with a daemon running restarts it, once and after the file is written, then
// confirms the new daemon uses Cortex. This is the case that used to look enabled and was
// not: the old daemon kept sending every prompt straight to OpenAI.
func TestCodexEnable_RestartsARunningDaemon(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	f := &fakeCodexDaemon{pid: 5613, env: offCortex, newPID: 7001, newEnv: onCortex}
	stubCodexDaemon(t, filepath.Dir(envPath), f)
	var wroteFirst bool
	prev := codexRestartDaemon
	codexRestartDaemon = func(home string, want map[string]string) error {
		_, err := os.Stat(envPath)
		wroteFirst = err == nil
		return prev(home, want)
	}

	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("enable exit %d: %s", code, errb.String())
	}
	got := out.String()
	wantAll(t, "enable output", got, "pid 5613", "Fri Oct 23 09:03:25 2026", "Applying this restarts it",
		"open Codex windows reconnect", "restarted (pid 7001) and is using Cortex")
	if len(f.restarts) != 1 {
		t.Fatalf("restarted %d times, want once", len(f.restarts))
	}
	if !wroteFirst {
		t.Error("restarted before the file was written, so the new daemon read the old one")
	}
	if f.restarts[0]["HTTPS_PROXY"] != cortexProxyURL {
		t.Errorf("restart was given want %v, which does not name Cortex's proxy", f.restarts[0])
	}
	// The warning is said before the question, so it can be declined.
	if strings.Index(got, "Applying this restarts it") > strings.Index(got, "Enabled —") {
		t.Errorf("the restart is announced after the change:\n%s", got)
	}
}

// Declining changes nothing, so nothing is restarted, though the warning was shown.
func TestCodexEnable_DeclinedRestartsNothing(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	f := &fakeCodexDaemon{pid: 5613, env: offCortex, newPID: 7001, newEnv: onCortex}
	stubCodexDaemon(t, filepath.Dir(envPath), f)
	prev := codexConfirm
	t.Cleanup(func() { codexConfirm = prev })
	codexConfirm = func(io.Writer) bool { return false }

	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), false, &out, &errb); code != exitDeclined {
		t.Fatalf("exit %d, want %d", code, exitDeclined)
	}
	wantAll(t, "declined enable output", out.String(), "Applying this restarts it", "Not changed.")
	if len(f.restarts) != 0 {
		t.Errorf("a declined enable restarted the daemon %d time(s)", len(f.restarts))
	}
}

// A restarted daemon that still has no Cortex proxy is named, not reported as fixed:
// something other than the file, such as the environment it was started from, wins.
func TestCodexEnable_SaysWhenTheRestartedDaemonStillBypassesCortex(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	f := &fakeCodexDaemon{pid: 5613, env: offCortex, newPID: 7001, newEnv: offCortex}
	stubCodexDaemon(t, filepath.Dir(envPath), f)

	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("enable exit %d: %s", code, errb.String())
	}
	wantAll(t, "enable output", out.String(), "restarted (pid 7001), but its proxy is none, not Cortex's")
}

// A restart that fails leaves the change made, so enable still exits 0, and stderr names
// the command that finishes the job.
func TestCodexEnable_RestartFailureIsReported(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	f := &fakeCodexDaemon{pid: 5613, env: offCortex, restartErr: errors.New("codex app-server daemon restart: exit status 1: busy")}
	stubCodexDaemon(t, filepath.Dir(envPath), f)

	var out, errb bytes.Buffer
	if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("enable exit %d, want 0: the file was changed", code)
	}
	wantAll(t, "stderr", errb.String(), "could not restart", "busy", codexDaemonRestartCmd)
	if readCodexEnv(t, envPath)["HTTPS_PROXY"] != cortexProxyURL {
		t.Error("the file was not enabled")
	}
}

// A restarted daemon that exits, or whose environment cannot be read, is said as such
// rather than claimed to be on Cortex.
func TestCodexEnable_RestartedDaemonUnchecked(t *testing.T) {
	for name, tc := range map[string]struct {
		f    fakeCodexDaemon
		want string
	}{
		"gone":       {fakeCodexDaemon{pid: 5613, env: offCortex, newGone: true}, "restarted, but is not running now"},
		"unreadable": {fakeCodexDaemon{pid: 5613, env: offCortex, newPID: 7001}, "restarted (pid 7001); its environment could not be checked"},
	} {
		t.Run(name, func(t *testing.T) {
			envPath, cfgPath := codexFixture(t, "")
			f := tc.f
			stubCodexDaemon(t, filepath.Dir(envPath), &f)
			var out, errb bytes.Buffer
			if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
				t.Fatalf("enable exit %d: %s", code, errb.String())
			}
			wantAll(t, "enable output", out.String(), tc.want)
		})
	}
}

// Disable restarts the daemon too, the other way round: a daemon left on Cortex would
// stop working once Cortex stops. Cortex's config is not read, so the restart is given no
// want, and the proxy it is compared with is the one the file held.
func TestCodexDisable_RestartsARunningDaemon(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	enableCodex(t, envPath, cfgPath)
	f := &fakeCodexDaemon{pid: 5613, env: onCortex, newPID: 7001, newEnv: offCortex}
	stubCodexDaemon(t, filepath.Dir(envPath), f)

	var out, errb bytes.Buffer
	if code := codexDisable(envPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("disable exit %d: %s", code, errb.String())
	}
	wantAll(t, "disable output", out.String(), "pid 5613", "Applying this restarts it", "restarted (pid 7001) and no longer uses Cortex")
	if len(f.restarts) != 1 || f.restarts[0] != nil {
		t.Errorf("restarts = %v, want one, given no want", f.restarts)
	}
}

// A daemon still on Cortex's proxy after disable got it from somewhere other than the
// file, and is told so.
func TestCodexDisable_SaysWhenTheRestartedDaemonStillUsesCortex(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	enableCodex(t, envPath, cfgPath)
	f := &fakeCodexDaemon{pid: 5613, env: onCortex, newPID: 7001, newEnv: onCortex}
	stubCodexDaemon(t, filepath.Dir(envPath), f)

	var out, errb bytes.Buffer
	if code := codexDisable(envPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
		t.Fatalf("disable exit %d: %s", code, errb.String())
	}
	wantAll(t, "disable output", out.String(), "still uses Cortex's proxy")
}

// Running enable again is what someone does when Codex does not seem to go through
// Cortex, and a daemon older than the file is the usual reason. With nothing to change it
// reports rather than restarts, and names the command.
func TestCodexEnable_AlreadyEnabledReportsTheDaemon(t *testing.T) {
	for name, tc := range map[string]struct {
		env  []string
		want string
	}{
		"old settings": {offCortex, "running with its old settings"},
		"on Cortex":    {onCortex, "(pid 5613) is using Cortex"},
	} {
		t.Run(name, func(t *testing.T) {
			envPath, cfgPath := codexFixture(t, "")
			enableCodex(t, envPath, cfgPath)
			f := &fakeCodexDaemon{pid: 5613, env: tc.env}
			stubCodexDaemon(t, filepath.Dir(envPath), f)

			var out, errb bytes.Buffer
			if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
				t.Fatalf("second enable exit %d: %s", code, errb.String())
			}
			wantAll(t, "second enable output", out.String(), "Already enabled", tc.want)
			if len(f.restarts) != 0 {
				t.Errorf("an enable with nothing to change restarted the daemon")
			}
		})
	}
}

// status says what the running daemon uses, since that, not the file, is what sends
// Codex's prompts.
func TestCodexStatus_ReportsTheDaemon(t *testing.T) {
	envPath, cfgPath := codexFixture(t, "")
	enableCodex(t, envPath, cfgPath)
	stubCodexDaemon(t, filepath.Dir(envPath), &fakeCodexDaemon{pid: 5613, env: offCortex})

	var out bytes.Buffer
	if code := codexStatus(envPath, cfgPath, &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	wantAll(t, "status output", out.String(), "enabled in "+envPath, "running with its old settings", codexDaemonRestartCmd)
}

// No daemon, or one whose pid file outlived it, needs nothing: the next codex starts a
// fresh daemon that reads the file enable just wrote.
func TestCodexEnable_NothingToRestartWithoutALiveDaemon(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, codexHome string){
		"no pid file": func(*testing.T, string) {},
		"stale pid file": func(t *testing.T, codexHome string) {
			writeCodexDaemonPID(t, codexHome, 5613)
		},
		"unreadable pid file": func(t *testing.T, codexHome string) {
			dir := filepath.Join(codexHome, filepath.Dir(codexDaemonPIDRel))
			_ = os.MkdirAll(dir, 0o700)
			_ = os.WriteFile(filepath.Join(codexHome, codexDaemonPIDRel), []byte("not json"), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			envPath, cfgPath := codexFixture(t, "")
			f := &fakeCodexDaemon{} // pid 0: nothing is alive
			stubCodexDaemon(t, filepath.Dir(envPath), f)
			setup(t, filepath.Dir(envPath))

			var out, errb bytes.Buffer
			if code := codexEnable(envPath, cfgPath, filepath.Join(filepath.Dir(envPath), "state.json"), true, &out, &errb); code != 0 {
				t.Fatalf("enable exit %d: %s", code, errb.String())
			}
			if strings.Contains(out.String(), "app-server") {
				t.Errorf("spoke of a daemon that is not running:\n%s", out.String())
			}
			if len(f.restarts) != 0 {
				t.Error("restarted a daemon that is not running")
			}
		})
	}
}

// Codex's own reading of the proxy variables, which is not Bun's: uppercase first, an
// empty value is unset, and ALL_PROXY is the fallback.
func TestCodexEnvironProxy(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"none", []string{"HOME=/h"}, ""},
		{"uppercase wins", []string{"https_proxy=http://lower:1", "HTTPS_PROXY=http://upper:2"}, "http://upper:2"},
		{"lowercase alone", []string{"https_proxy=http://lower:1"}, "http://lower:1"},
		{"empty uppercase hides lowercase", []string{"HTTPS_PROXY=", "https_proxy=http://lower:1"}, ""},
		{"ALL_PROXY fallback", []string{"ALL_PROXY=http://all:3"}, "http://all:3"},
		{"HTTP_PROXY is not for HTTPS", []string{"HTTP_PROXY=http://plain:4"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexEnvironProxy(tc.env); got != tc.want {
				t.Errorf("codexEnvironProxy(%q) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

// The restart's environment loses Cortex's values and keeps everything else, a proxy of
// the user's own included, so a restart after disable cannot put the daemon back on
// Cortex from the shell it ran in.
func TestCodexCLIEnv(t *testing.T) {
	env := []string{"HOME=/h", "HTTPS_PROXY=" + cortexProxyURL, "http_proxy=http://corp.example.com:3128", "SSL_CERT_FILE=/h/.cortex/ca/bundle.crt"}
	got := strings.Join(codexCLIEnv(env, nil), " ")
	if want := "HOME=/h http_proxy=http://corp.example.com:3128"; got != want {
		t.Errorf("codexCLIEnv = %q, want %q", got, want)
	}
}

// The real runner, given /bin/sh rather than codex: a failure carries stderr's first
// line, the child gets no Cortex proxy, and CODEX_HOME names a home Codex would not find
// by itself.
func TestCodexRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv(codexHomeEnv, "")
	t.Setenv("HTTPS_PROXY", cortexProxyURL)
	home := t.TempDir()
	err := codexRun("/bin/sh", home, nil, "-c", `echo "home=$CODEX_HOME proxy=$HTTPS_PROXY" >&2; echo second >&2; exit 3`)
	if err == nil {
		t.Fatal("a non-zero exit returned no error")
	}
	if msg := err.Error(); !strings.HasSuffix(msg, ": exit status 3: home="+home+" proxy=") {
		t.Errorf("err = %q, want it to end with the exit status, CODEX_HOME and no proxy", msg)
	}
	if err := codexRun("/bin/sh", home, nil, "-c", "exit 0"); err != nil {
		t.Errorf("a zero exit returned %v", err)
	}
}

// codex is found on PATH, else where its standalone installer puts it.
func TestFindCodex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	t.Setenv("PATH", t.TempDir())
	home := t.TempDir()
	if _, err := findCodex(home); err == nil {
		t.Error("found a codex that is not there")
	}
	bin := filepath.Join(home, "packages", "standalone", "current", "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := findCodex(home); err != nil || got != bin {
		t.Errorf("findCodex = %q, %v; want %q", got, err, bin)
	}
}

// The liveness check against the real process table: this process is alive, a pid no
// process can have is not.
func TestCodexDaemonAlive_RealProcesses(t *testing.T) {
	if !codexDaemonAlive(os.Getpid()) {
		t.Error("this test's own process reads as not running")
	}
	if codexDaemonAlive(0) || codexDaemonAlive(-1) {
		t.Error("a non-positive pid reads as running")
	}
}
