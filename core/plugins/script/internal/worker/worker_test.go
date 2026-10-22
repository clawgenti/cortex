package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary is its own fake script. With FAKE_WORKER set, TestMain runs the
// named behaviour instead of the tests, so nothing here needs Python or a shell.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_WORKER"); mode != "" {
		fakeWorker(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeWorker(mode string) {
	out := bufio.NewWriter(os.Stdout)
	send := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(append(b, '\n'))
		out.Flush()
	}
	switch mode {
	case "no-handshake":
		fmt.Fprintln(out, "hello, I am not a handshake")
		out.Flush()
		time.Sleep(time.Hour)
	case "wrong-protocol":
		send(map[string]any{"cortex": 99})
		time.Sleep(time.Hour)
	case "exit-at-start":
		os.Exit(3)
	}
	fmt.Fprintln(out, "starting up") // stray output before the handshake is skipped
	send(Handshake{Cortex: Protocol, Hooks: []string{"request"}, Enforce: mode == "enforce"})
	if mode == "deaf" {
		time.Sleep(time.Hour) // handshakes, then never reads stdin
	}
	if mode == "spawn-child" {
		// A grandchild in the same process group; the test checks a timeout kills it.
		child := exec.Command("sleep", "3600")
		_ = child.Start()
		os.WriteFile(os.Getenv("FAKE_PIDFILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 64<<20)
	count := 0
	for in.Scan() {
		var msg map[string]any
		json.Unmarshal(in.Bytes(), &msg)
		count++
		switch mode {
		case "hang", "spawn-child":
			time.Sleep(time.Hour)
		case "crash-after-one":
			if count > 1 {
				os.Exit(2)
			}
		case "stale-id":
			send(map[string]any{"id": -1, "stale": true})
		}
		fmt.Fprintln(out, "log line", count) // the script's own output, between replies
		send(map[string]any{"id": msg["id"], "count": count, "echo": msg["echo"]})
	}
}

func fake(t *testing.T, mode string, extraEnv ...string) Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		Path:         exe,
		Env:          append(append(os.Environ(), "FAKE_WORKER="+mode), extraEnv...),
		StartTimeout: 5 * time.Second,
		CallTimeout:  2 * time.Second,
	}
}

// call makes one call, starting w first: a Call never waits for a start.
func call(t *testing.T, w *Worker, echo string) (map[string]any, Reply) {
	t.Helper()
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r, err := w.Call(context.Background(), map[string]any{"echo": echo})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(r.Line, &got); err != nil {
		t.Fatalf("reply %q: %v", r.Line, err)
	}
	return got, r
}

func TestWorker_HandshakeAndCalls(t *testing.T) {
	w := New(fake(t, "enforce"))
	t.Cleanup(w.Stop)
	hs, err := w.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !hs.Enforce || !hs.Has("request") || hs.Has("response") {
		t.Errorf("handshake = %+v", hs)
	}
	for i := 1; i <= 3; i++ {
		got, r := call(t, w, "hi")
		if got["count"] != float64(i) || got["echo"] != "hi" {
			t.Errorf("call %d: reply %v", i, got)
		}
		// State lives in the process: the count rises, so it is one process throughout.
		if len(r.Logs) != 1 || r.Logs[0] != "log line "+strconv.Itoa(i) {
			t.Errorf("call %d: logs %q", i, r.Logs)
		}
	}
}

// A Call to a worker never started starts it in the background and fails at once,
// so a script that is slow to start does not hold a request up.
func TestWorker_CallStartsTheProcessInTheBackground(t *testing.T) {
	w := New(fake(t, "plain"))
	t.Cleanup(w.Stop)
	if _, err := w.Call(context.Background(), map[string]any{}); !errors.Is(err, ErrStarting) {
		t.Fatalf("first Call = %v, want ErrStarting", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := w.Call(context.Background(), map[string]any{}); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the background start never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWorker_HandshakeFailures(t *testing.T) {
	for mode, want := range map[string]string{
		"no-handshake":   "no handshake",
		"wrong-protocol": "protocol 99",
		"exit-at-start":  "no handshake",
	} {
		t.Run(mode, func(t *testing.T) {
			cfg := fake(t, mode)
			cfg.StartTimeout = 300 * time.Millisecond
			w := New(cfg)
			t.Cleanup(w.Stop)
			_, err := w.Start(context.Background())
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Start error = %v, want one mentioning %q", err, want)
			}
		})
	}
}

// A reply carrying another call's id is the script's business, not this call's reply:
// it is kept as output and the real reply is still found.
func TestWorker_IgnoresARepliesWithAnotherID(t *testing.T) {
	w := New(fake(t, "stale-id"))
	t.Cleanup(w.Stop)
	got, r := call(t, w, "x")
	if got["echo"] != "x" {
		t.Errorf("reply %v", got)
	}
	if len(r.Logs) != 2 || !strings.Contains(r.Logs[0], "stale") {
		t.Errorf("logs %q", r.Logs)
	}
}

func TestWorker_TimeoutKillsAndRestarts(t *testing.T) {
	cfg := fake(t, "hang")
	cfg.CallTimeout = 200 * time.Millisecond
	w := New(cfg)
	t.Cleanup(w.Stop)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := w.Call(context.Background(), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "no reply in time") {
		t.Fatalf("Call error = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("timed-out call took %v", d)
	}
	// Backing off: refused immediately rather than started again.
	if _, err := w.Call(context.Background(), map[string]any{}); !errors.Is(err, ErrBackoff) {
		t.Errorf("second Call error = %v, want ErrBackoff", err)
	}
}

func TestWorker_TimeoutKillsTheWholeProcessGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	cfg := fake(t, "spawn-child", "FAKE_PIDFILE="+pidfile)
	cfg.CallTimeout = 200 * time.Millisecond
	w := New(cfg)
	t.Cleanup(w.Stop)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Call(context.Background(), map[string]any{}); err == nil {
		t.Fatal("call to a hanging script succeeded")
	}
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(b))
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWorker_CrashThenRestartAfterBackoff(t *testing.T) {
	w := New(fake(t, "crash-after-one"))
	t.Cleanup(w.Stop)
	var clockMu sync.Mutex
	clock := time.Now()
	w.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	call(t, w, "first")
	if _, err := w.Call(context.Background(), map[string]any{}); err == nil {
		t.Fatal("call to a crashing script succeeded")
	}
	if _, err := w.Call(context.Background(), map[string]any{}); !errors.Is(err, ErrBackoff) {
		t.Fatalf("call during backoff = %v, want ErrBackoff", err)
	}
	clockMu.Lock()
	clock = clock.Add(minBackoff + time.Millisecond)
	clockMu.Unlock()
	// After the backoff the restart runs in the background: the call that finds no
	// child fails at once instead of waiting for it.
	if _, err := w.Call(context.Background(), map[string]any{}); !errors.Is(err, ErrStarting) {
		t.Fatalf("call after backoff = %v, want ErrStarting", err)
	}
	// A new process: its count starts over, which is the state loss a crash costs.
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := w.Call(context.Background(), map[string]any{"echo": "again"})
		if err == nil {
			var got map[string]any
			json.Unmarshal(r.Line, &got)
			if got["count"] != float64(1) {
				t.Errorf("after restart, reply %v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never restarted: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWorker_StopRefusesCalls(t *testing.T) {
	w := New(fake(t, "plain"))
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	call(t, w, "x")
	w.Stop()
	if _, err := w.Call(context.Background(), map[string]any{}); err == nil {
		t.Fatal("Call after Stop succeeded")
	}
}

func TestParseLoginPath(t *testing.T) {
	for out, want := range map[string]string{
		"Welcome!\nnvm loaded\n\n" + pathMarker + "/opt/homebrew/bin:/usr/bin" + pathMarker + "\n": "/opt/homebrew/bin:/usr/bin",
		"no markers at all":            "",
		pathMarker + "only one marker": "",
	} {
		if got := parseLoginPath(out); got != want {
			t.Errorf("parseLoginPath(%q) = %q, want %q", out, got, want)
		}
	}
}

// A login shell whose profile prints still yields its PATH.
func TestLoginPath_ProfileThatPrints(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	shell := filepath.Join(dir, "chatty-shell")
	// A shell that greets before running what it was asked to.
	os.WriteFile(shell, []byte("#!"+sh+"\necho 'Welcome to the machine'\nshift\nPATH=/from/login exec "+sh+" \"$@\"\n"), 0o700)
	t.Setenv("SHELL", shell)
	loginPathMu.Lock()
	loginPathVal, loginPathTried = "", time.Time{}
	old := loginPathTimeout
	loginPathTimeout = 30 * time.Second
	loginPathMu.Unlock()
	t.Cleanup(func() { loginPathVal, loginPathTried, loginPathTimeout = "", time.Now(), old })
	if got := loginPath(); got != "/from/login" {
		t.Errorf("loginPath() = %q, want /from/login", got)
	}
}

func TestEnviron(t *testing.T) {
	loginPathMu.Lock()
	loginPathVal, loginPathTried = "", time.Now() // no login shell in a test: use the base PATH
	loginPathMu.Unlock()
	got := Environ([]string{
		"HOME=/h", "HTTPS_PROXY=http://127.0.0.1:47600", "http_proxy=x", "NO_PROXY=a",
		"PATH=/usr/bin", "PYTHONPATH=/mine",
	}, "/sdk")
	want := []string{"HOME=/h", "PATH=/usr/bin", "PYTHONPATH=/sdk:/mine"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("Environ = %q, want %q", got, want)
	}
}

// waitStopped waits for w to refuse calls; a released worker stops in the background.
func waitStopped(t *testing.T, w *Worker, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := w.Call(context.Background(), map[string]any{})
		if err != nil && strings.Contains(err.Error(), "stopped") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still answers", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPool_ReusesAnUnchangedScript(t *testing.T) {
	p := NewPool()
	cfg := fake(t, "plain")
	one := p.NewHolder()
	a := p.Acquire(one, cfg, "h1")
	t.Cleanup(a.Stop)
	call(t, a, "x")
	p.Activate(one)
	// The next build, same file: the same worker, with its state.
	two := p.NewHolder()
	if b := p.Acquire(two, cfg, "h1"); b != a {
		t.Fatal("an unchanged script got a new worker")
	}
	p.Activate(two)
	p.Release(one) // the old pipeline is drained
	if got, _ := call(t, a, "y"); got["count"] != float64(2) {
		t.Errorf("state lost across builds: %v", got)
	}
	p.Release(two)
	waitStopped(t, a, "a worker no build holds")
}

// An edited script gets a new worker, and the old one keeps serving the running
// pipeline until that pipeline is released — not merely until the new build is made,
// which may still be refused.
func TestPool_AnEditedScriptKeepsItsOldWorkerUntilTheOldBuildIsReleased(t *testing.T) {
	p := NewPool()
	cfg := fake(t, "plain")
	one := p.NewHolder()
	old := p.Acquire(one, cfg, "h1")
	p.Activate(one)
	two := p.NewHolder()
	edited := p.Acquire(two, cfg, "h2")
	t.Cleanup(edited.Stop)
	if edited == old {
		t.Fatal("an edited script kept its old worker")
	}
	call(t, old, "still serving") // the running pipeline still works
	p.Activate(two)
	call(t, old, "draining")
	p.Release(one)
	waitStopped(t, old, "the old version")
	call(t, edited, "new")
}

// A build that is configured and then refused never starts and is never shut down.
// The next build to start sweeps what it held; the running build keeps its workers.
func TestPool_AnAbandonedBuildIsSweptWhenALaterOneStarts(t *testing.T) {
	p := NewPool()
	cfg := fake(t, "plain")
	running := p.NewHolder()
	kept := p.Acquire(running, cfg, "h1")
	t.Cleanup(kept.Stop)
	p.Activate(running)

	refused := p.NewHolder()
	orphan := p.Acquire(refused, cfg, "h2") // never activated, never released
	call(t, orphan, "x")

	next := p.NewHolder()
	p.Acquire(next, cfg, "h1")
	p.Activate(next)
	waitStopped(t, orphan, "a refused build's worker")
	call(t, kept, "the running build's worker")
}

// The call deadline bounds the whole exchange, the write included. A script that
// stops reading fills the pipe; a body larger than the pipe buffer — which every
// inference request is — must still time out rather than block the request.
func TestWorker_TimeoutCoversTheWrite(t *testing.T) {
	cfg := fake(t, "deaf")
	cfg.CallTimeout = 200 * time.Millisecond
	w := New(cfg)
	t.Cleanup(w.Stop)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := w.Call(context.Background(), map[string]any{"body": strings.Repeat("x", 2<<20)})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a call to a script that does not read succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the call blocked past its 200ms deadline")
	}
	// The worker is not wedged: the next call is answered at once, not queued
	// behind a write that never finishes.
	start := time.Now()
	w.Call(context.Background(), map[string]any{})
	if d := time.Since(start); d > time.Second {
		t.Errorf("the next call waited %v behind the stuck one", d)
	}
}
