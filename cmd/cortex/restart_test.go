package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// runMainEnv makes a re-executed copy of this test binary run something other than the
// tests. These tests are about what the process does with its ports and its data on disk,
// which no function short of main() decides, so they run main() itself in a child.
const runMainEnv = "CORTEX_TEST_RUN"

func TestMain(m *testing.M) {
	switch os.Getenv(runMainEnv) {
	case "main":
		main()
		os.Exit(0)
	case "stuck-child":
		// The supervisor re-executes this binary without -supervise; that copy stands in for
		// a proxy whose shutdown hangs, so the supervisor's own deadline is what ends it.
		if slices.Contains(os.Args[1:], "-supervise") {
			main()
			os.Exit(0)
		}
		signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
		fmt.Fprintln(os.Stderr, "stuck child: ignoring SIGTERM")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// lockedBuffer collects a child's output while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

type proxyFixture struct {
	home, config string
}

// newProxyFixture writes a forward-only, loopback-only config. local puts it where a local
// install's is — under $HOME/.cortex, which turns the session archive and the cost ledger
// on — and !local somewhere else, which is how a pod's config looks to this binary.
func newProxyFixture(t *testing.T, local bool) proxyFixture {
	t.Helper()
	f := proxyFixture{home: t.TempDir()}
	dir := filepath.Join(f.home, cortexDirName)
	if !local {
		dir = filepath.Join(f.home, "elsewhere")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.config = filepath.Join(dir, "config.yaml")
	writeProxyConfig(t, f.config, anyPort)
	return f
}

// anyPort has the kernel pick the port when the child binds. Picking one here and handing it
// over raced: a port free when the test asked could be taken by the time the child bound
// it, which failed the child with "already in use" (#1452). startProxy reads back where the
// child landed.
const anyPort = "127.0.0.1:0"

// writeProxyConfig writes the fixture's config to path with the forward proxy on forward and
// every other listener on anyPort.
func writeProxyConfig(t *testing.T, path, forward string) {
	t.Helper()
	body := fmt.Sprintf(`mode: proxy-sidecar
listener:
  roles: [forward]
  bind_loopback_only: true
  forward_proxy_addr: %s
  session_api_addr: %s
  health_addr: %s
  transparent_proxy_addr: %s
stats:
  address: %s
pipeline:
  inbound: {plugins: []}
  outbound: {plugins: []}
`, forward, anyPort, anyPort, anyPort, anyPort)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// childEnv is this environment with HOME moved and anything that would route the child's
// traffic through, or make it trust, a Cortex the developer is running dropped.
func childEnv(home, run string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "HOME", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "SSL_CERT_FILE", runMainEnv:
			continue
		}
		env = append(env, kv)
	}
	return append(env, "HOME="+home, runMainEnv+"="+run)
}

type child struct {
	cmd  *exec.Cmd
	out  *lockedBuffer
	done chan error
}

func startChild(t *testing.T, home, run string, args ...string) *child {
	t.Helper()
	c := &child{out: &lockedBuffer{}, done: make(chan error, 1)}
	c.cmd = exec.Command(os.Args[0], args...) //nolint:gosec // this test binary
	c.cmd.Env = childEnv(home, run)
	c.cmd.Stdout, c.cmd.Stderr = c.out, c.out
	// Its own process group, as under launchd, so cleanup can end a supervisor's child too:
	// one left behind would hold the output pipe open and keep Wait from returning.
	c.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { c.done <- c.cmd.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
		<-c.done
		if t.Failed() {
			t.Logf("child output:\n%s", c.out.String())
		}
	})
	return c
}

// proxy is a running child and the forward proxy address it bound.
type proxy struct {
	*child
	forward string
}

// Where the child's servers landed, read off the lines each logs as it starts serving.
var (
	healthListening  = regexp.MustCompile(`msg="health server listening" [^\n]*\baddr=(\S+)`)
	forwardListening = regexp.MustCompile(`msg="HTTP server listening" [^\n]*\bname=forward-proxy addr=(\S+)`)
)

func startProxy(t *testing.T, f proxyFixture) proxy {
	t.Helper()
	c := startChild(t, f.home, "main", "-config", f.config)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out := c.out.String()
		if h, fw := healthListening.FindStringSubmatch(out), forwardListening.FindStringSubmatch(out); h != nil && fw != nil {
			if resp, err := http.Get("http://" + h[1] + "/healthz"); err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return proxy{child: c, forward: fw[1]}
				}
			}
		}
		select {
		case err := <-c.done:
			c.done <- err
			t.Fatalf("proxy exited before it was healthy: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("proxy did not become healthy")
	return proxy{}
}

// waitExit returns how long the child took to exit after since, failing at within.
func (c *child) waitExit(t *testing.T, since time.Time, within time.Duration) time.Duration {
	t.Helper()
	select {
	case err := <-c.done:
		c.done <- err
		return time.Since(since)
	case <-time.After(within - time.Since(since)):
		t.Fatalf("did not exit within %s", within)
		return 0
	}
}

// hangThroughProxy sends a plain-HTTP request through the forward proxy to an upstream
// that never answers, and returns once the upstream holds it — a request in flight.
func hangThroughProxy(t *testing.T, forward string) {
	t.Helper()
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	up := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = up.Serve(ln) }()
	t.Cleanup(func() { close(release); _ = up.Close() })

	proxyURL, _ := url.Parse("http://" + forward)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	go func() {
		if resp, err := client.Get("http://" + ln.Addr().String() + "/hang"); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached the upstream through the proxy")
	}
}

// A second proxy started over a running one must give up on the ports BEFORE it touches
// ~/.cortex. Opening the session archive is not read-only — it removes trash, prunes, and
// is closed on the fatal path — and the running proxy is writing the same files. On
// 2026-10-07 two supervised children did exactly that against an orphaned proxy, replaying
// segments it had not finished (incompleteSegments=11), before each died on the bind.
func TestSecondInstanceExitsBeforeOpeningData(t *testing.T) {
	f := newProxyFixture(t, true)
	first := startProxy(t, f)
	if !strings.Contains(first.out.String(), "session archive enabled") {
		t.Fatalf("fixture is not a local install: the first proxy did not open an archive")
	}

	// Its own config beside the first's, so it can name the port the first one bound: the
	// forward proxy is the first socket it reserves.
	over := filepath.Join(filepath.Dir(f.config), "over.yaml")
	writeProxyConfig(t, over, first.forward)
	second := startChild(t, f.home, "main", "-config", over)
	second.waitExit(t, time.Now(), 15*time.Second)
	out := second.out.String()
	if second.cmd.ProcessState.Success() {
		t.Fatalf("second proxy exited 0 over a running one")
	}
	for _, opened := range []string{"session archive enabled", "cost ledger enabled"} {
		if strings.Contains(out, opened) {
			t.Errorf("second proxy logged %q before giving up on the ports", opened)
		}
	}
	if !strings.Contains(out, "already in use") {
		t.Errorf("second proxy did not say a port was taken:\n%s", out)
	}
}

// A local install stops at once: it releases every port and exits rather than draining the
// requests in flight, which its clients retry. A drain made every restart wait out the
// longest open request — a stream that never ends held it to its 15s deadline, long enough
// for launchd's 5s exit timeout to kill the supervisor and orphan the proxy on its ports.
func TestLocalInstallStopsWithoutDraining(t *testing.T) {
	f := newProxyFixture(t, true)
	p := startProxy(t, f)
	hangThroughProxy(t, p.forward)

	sent := time.Now()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	took := p.waitExit(t, sent, 2*time.Second)
	t.Logf("exited %s after SIGTERM with a request in flight", took.Round(time.Millisecond))
	if !p.cmd.ProcessState.Success() {
		t.Errorf("exit was not clean: %v", p.cmd.ProcessState)
	}
}

// Anywhere else — a pod — the drain stays: endpoint removal propagates after SIGTERM, so
// callers are still being routed here while it runs.
func TestPodStillDrains(t *testing.T) {
	f := newProxyFixture(t, false)
	p := startProxy(t, f)
	hangThroughProxy(t, p.forward)

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		p.done <- err
		t.Fatalf("a pod's proxy exited at once with a request in flight: %v", err)
	case <-time.After(time.Second):
	}
}

// The supervisor ends a proxy that will not stop inside launchd's exit timeout (5s, pinned
// as ExitTimeOut in the plist agentop writes). Waiting longer is what let launchd SIGKILL
// the supervisor mid-wait: launchd only SIGTERMs the rest of the process group, which a
// proxy that has already taken its one SIGTERM ignores, so it lived on holding its ports.
func TestSupervisorEndsAStuckProxyInsideLaunchdsWindow(t *testing.T) {
	home := t.TempDir()
	sup := startChild(t, home, "stuck-child", "-supervise")
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(sup.out.String(), "stuck child: ignoring SIGTERM") {
		if time.Now().After(deadline) {
			t.Fatal("the supervised child never started")
		}
		time.Sleep(20 * time.Millisecond)
	}

	sent := time.Now()
	if err := sup.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	took := sup.waitExit(t, sent, 5*time.Second)
	t.Logf("supervisor exited %s after SIGTERM", took.Round(time.Millisecond))
	if !strings.Contains(sup.out.String(), "killing it") {
		t.Errorf("supervisor exited without killing the stuck proxy:\n%s", sup.out.String())
	}
}
