# Supervisor Fallback Listener Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** While no proxy child is serving, the Cortex supervisor holds the forward address and refuses every connection with a `502` that names Cortex and the command that fixes it, so a routed agent stops seeing a bare connection-refused.

**Architecture:** `runSupervisor` in `cmd/cortex/supervise.go` is a re-exec loop that restarts the proxy forever. It gains a `fallbackServer` that it opens whenever no child is running and closes immediately before starting one, so a healthy install is unchanged. The refusal is written on the raw socket because Go's `net/http` cannot set a custom reason phrase, and both the reason phrase and the body are load-bearing for different clients.

**Tech Stack:** Go 1.26.5, stdlib only (`net`, `bufio`, `log/slog`), `github.com/rossoctl/cortex/core/config` for the address. No new dependency.

**Spec:** `docs/superpowers/specs/2026-10-13-supervisor-fallback-listener-design.md`

## Global Constraints

- **Module:** all work is in `cmd/cortex`, module `github.com/rossoctl/cortex/cmd/cortex`. Run every Go command from that directory with `GOWORK=off`, as CI does for this module.
- **No `core/` change, no agentop change, no config surface, no new flag.** The spec lists these as non-goals.
- **Fail-closed.** The fallback never forwards, never dials upstream and holds no CA. It answers and closes.
- **Both response fields are load-bearing.** Claude Code reads the body, Go `net/http` reads the reason phrase, `curl` discards both. Never replace the raw write with `http.Error` — Go always writes the canonical status text and the Go-visible half would be lost silently.
- **Body is exactly one line with no trailing newline.** A trailing newline rendered as a stray `.` on its own line inside Claude Code's error framing.
- **Default address is `127.0.0.1:47600`**, matching `cmd/agentop/setup_step_service.go:146`. The config key is `listener.forward_proxy_addr` (`config.ListenerConfig.ForwardProxyAddr`, `core/config/config.go:779`) — *not* agentop's `servicePaths.forwardAddr`, a different field.
- **Attribution:** every commit uses `git commit -s` and ends with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Never `Co-Authored-By`. Never pass `-S` or enable GPG signing; use `git -c commit.gpgsign=false commit` if signing is configured globally.
- **Formatting is not enforced by CI**, so run `gofmt -l .` (with a path — a bare `gofmt -l` reads stdin and scans nothing) and `GOWORK=off go vet ./...` yourself before the final commit.

---

### Task 1: Resolve the forward address from argv

The supervisor has argv but has never parsed config. It needs the address to bind, and it needs an answer in exactly the case the config is unreadable — a config that will not load is one of the failure modes this feature exists to cover.

**Files:**
- Create: `cmd/cortex/fallback.go`
- Test: `cmd/cortex/fallback_test.go`

**Interfaces:**
- Consumes: `config.Load(path string) (*config.Config, error)` from `github.com/rossoctl/cortex/core/config`; `cfg.Listener.ForwardProxyAddr`.
- Produces: `configPathFromArgs(args []string) string`, `supervisorForwardAddr(args []string) string`, `const fallbackDefaultAddr = "127.0.0.1:47600"`. Task 3 calls `supervisorForwardAddr(args)`.

- [ ] **Step 1: Write the failing test**

Create `cmd/cortex/fallback_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The supervisor never parsed config before this, so it learns the address the same
// way it learns everything else: from the argv it is about to hand the child. Both
// spellings and both forms, because main.go uses flag.String and so accepts all four.
func TestConfigPathFromArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"separate single dash", []string{"-config", "/etc/c.yaml"}, "/etc/c.yaml"},
		{"separate double dash", []string{"--config", "/etc/c.yaml"}, "/etc/c.yaml"},
		{"joined single dash", []string{"-config=/etc/c.yaml"}, "/etc/c.yaml"},
		{"joined double dash", []string{"--config=/etc/c.yaml"}, "/etc/c.yaml"},
		{"among others", []string{"-local", "--config", "/etc/c.yaml", "-ca-dir", "/d"}, "/etc/c.yaml"},
		{"absent", []string{"-local"}, ""},
		{"dangling value", []string{"-local", "--config"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := configPathFromArgs(tc.args); got != tc.want {
				t.Errorf("configPathFromArgs(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// Every miss has to yield an address, not an error. The whole point of the fallback is
// to speak when the proxy cannot start, and "the config will not load" is one of the
// reasons it cannot start -- so that case must not be the one where we fall silent.
func TestSupervisorForwardAddr_FallsBackWhereverTheConfigCannotAnswer(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.yaml")
	if err := os.WriteFile(good, []byte("mode: proxy-sidecar\nlistener:\n  forward_proxy_addr: 127.0.0.1:48123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(broken, []byte("listener: [this is not a mapping\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"config names an address", []string{"--config", good}, "127.0.0.1:48123"},
		{"config will not parse", []string{"--config", broken}, fallbackDefaultAddr},
		{"config is not there", []string{"--config", filepath.Join(dir, "absent.yaml")}, fallbackDefaultAddr},
		{"no config flag at all", []string{"-local"}, fallbackDefaultAddr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := supervisorForwardAddr(tc.args); got != tc.want {
				t.Errorf("supervisorForwardAddr(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd cmd/cortex && GOWORK=off go test -run 'TestConfigPathFromArgs|TestSupervisorForwardAddr' ./... 2>&1 | head -20
```

Expected: FAIL to build — `undefined: configPathFromArgs`, `undefined: supervisorForwardAddr`, `undefined: fallbackDefaultAddr`.

- [ ] **Step 3: Write the minimal implementation**

Create `cmd/cortex/fallback.go`:

```go
package main

import (
	"log/slog"
	"strings"

	"github.com/rossoctl/cortex/core/config"
)

// fallbackDefaultAddr is where the forward proxy is pinned for a local install. It is
// the answer whenever the config cannot give one, which is not a rare path: a config
// that will not load is one of the reasons the proxy cannot start, and that is exactly
// when the fallback has something to say. Kept equal to the default in
// cmd/agentop/setup_step_service.go, which picks it for the same reason.
const fallbackDefaultAddr = "127.0.0.1:47600"

// configPathFromArgs finds the -config value in the arguments the supervisor is about
// to hand the child. main.go declares it with flag.String, so all four spellings are
// accepted and all four are read here.
func configPathFromArgs(args []string) string {
	for i, a := range args {
		switch {
		case a == "-config" || a == "--config":
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		case strings.HasPrefix(a, "-config="):
			return strings.TrimPrefix(a, "-config=")
		case strings.HasPrefix(a, "--config="):
			return strings.TrimPrefix(a, "--config=")
		}
	}
	return ""
}

// supervisorForwardAddr is the address the fallback listener binds: the forward proxy's
// own, so a client routed at it reaches the fallback when no child holds it. Every miss
// falls back rather than failing, for the reason on fallbackDefaultAddr.
func supervisorForwardAddr(args []string) string {
	path := configPathFromArgs(args)
	if path == "" {
		return fallbackDefaultAddr
	}
	cfg, err := config.Load(path)
	if err != nil {
		slog.Debug("supervisor: cannot read the config for the fallback address; using the default",
			"config", path, "addr", fallbackDefaultAddr, "error", err)
		return fallbackDefaultAddr
	}
	if cfg.Listener.ForwardProxyAddr == "" {
		return fallbackDefaultAddr
	}
	return cfg.Listener.ForwardProxyAddr
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
cd cmd/cortex && GOWORK=off go test -run 'TestConfigPathFromArgs|TestSupervisorForwardAddr' ./... 2>&1 | tail -5
```

Expected: PASS. If `config names an address` fails because `config.Load` rejects the minimal YAML, read the error and add only the keys `Load` demands to the fixture — do not change the assertion.

- [ ] **Step 5: Commit**

```bash
cd /Users/cwiklik/dev/aiplatform/public/cortex/.worktrees/down-breaks-harness
gofmt -l cmd/cortex
git add cmd/cortex/fallback.go cmd/cortex/fallback_test.go
git -c commit.gpgsign=false commit -s -F - <<'EOF'
feat: Resolve the forward address for the supervisor fallback

The supervisor has argv but has never parsed config. It needs the forward
address to bind a fallback listener, and it needs an answer in exactly the
case the config is unreadable, because a config that will not load is one
of the reasons the proxy cannot start.

Refs #1002

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 2: The fail-closed listener and its refusal

**Files:**
- Modify: `cmd/cortex/fallback.go`
- Test: `cmd/cortex/fallback_test.go`

**Interfaces:**
- Consumes: `fallbackDefaultAddr` from Task 1.
- Produces: `type fallbackServer struct{...}` with `open(addr string)`, `openOn(ln net.Listener)` and `close()`; `const fallbackReason`, `const fallbackBody`; `var fallbackRefusal []byte`. Task 3 uses `open` and `close` only; `openOn` exists so tests can hand in a listener on port 0 instead of racing for a free port.

- [ ] **Step 1: Write the failing test**

Append to `cmd/cortex/fallback_test.go` (and add `"bufio"`, `"io"`, `"net"`, `"net/http"`, `"net/url"`, `"strconv"`, `"strings"`, `"time"` to its imports):

```go
// The exact bytes, because two different fields of this response are read by two
// different clients and a plausible-looking refactor can drop either without any other
// test noticing. Measured against real clients: Claude Code prints the body, Go's
// net/http returns the reason phrase as the error, curl discards both.
func TestFallbackRefusal_Bytes(t *testing.T) {
	got := string(fallbackRefusal)
	want := "HTTP/1.1 502 " + fallbackReason + "\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Length: " + strconv.Itoa(len(fallbackBody)) + "\r\n" +
		"Connection: close\r\n" +
		"Proxy-Connection: close\r\n" +
		"\r\n" + fallbackBody
	if got != want {
		t.Errorf("refusal =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(fallbackBody, "\n") {
		t.Error("the body spans lines; a trailing newline renders as a stray '.' inside Claude Code's error framing")
	}
	if !strings.Contains(fallbackBody, "agentop service start") {
		t.Error("the body does not name the command that fixes it, which is the whole point of it")
	}
}

// A Go client reads the reason phrase and nothing else. This is the half that would be
// lost by rewriting the response as http.Error, which always writes the canonical
// status text -- a change that would still pass every other test here.
func TestFallbackServer_GoClientSeesTheReasonPhrase(t *testing.T) {
	addr := startFallback(t)

	pu, err := url.Parse("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(pu)},
		Timeout:   5 * time.Second,
	}
	_, err = c.Get("https://cortex.invalid/") //nolint:noctx // bounded by Timeout
	if err == nil {
		t.Fatal("the request succeeded; the fallback must refuse every connection")
	}
	if !strings.Contains(err.Error(), fallbackReason) {
		t.Errorf("go client error = %v, want it to carry the reason phrase %q", err, fallbackReason)
	}
}

// Claude Code reads the body. Asserted over a raw CONNECT because that is what an agent
// routed at an HTTPS endpoint actually sends.
func TestFallbackServer_ConnectGetsTheBody(t *testing.T) {
	addr := startFallback(t)

	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.HasPrefix(got, "HTTP/1.1 502 "+fallbackReason) {
		t.Errorf("status line = %q, want the 502 and the reason phrase", strings.SplitN(got, "\r\n", 2)[0])
	}
	if !strings.HasSuffix(got, fallbackBody) {
		t.Errorf("response does not end in the body; got %q", got)
	}
}

// close() has to actually free the address, because the supervisor closes the fallback
// precisely so the child can bind it. A close that leaked the port would turn this
// feature into the port conflict of #1122 on every single restart.
func TestFallbackServer_CloseFreesTheAddress(t *testing.T) {
	addr := startFallback(t)
	// startFallback registered the close; do it now and re-bind.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the address is still held after close: %v", err)
	}
	_ = ln.Close()
}

// startFallback opens a fallback server on a port the kernel picks and returns its
// address. openOn rather than open so there is no window between finding a free port
// and binding it.
func startFallback(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fb := &fallbackServer{}
	fb.openOn(ln)
	addr := ln.Addr().String()
	t.Cleanup(fb.close)
	return addr
}
```

`TestFallbackServer_CloseFreesTheAddress` relies on `t.Cleanup` running after the test body, so move the close before the re-bind by replacing its body with this (the version above would re-bind while the server is still open):

```go
func TestFallbackServer_CloseFreesTheAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fb := &fallbackServer{}
	fb.openOn(ln)
	addr := ln.Addr().String()

	fb.close()

	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the address is still held after close: %v — the child could not bind it", err)
	}
	_ = again.Close()

	fb.close() // idempotent: the supervisor closes before every start, including the first
}
```

Use the second version and delete the first.

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd cmd/cortex && GOWORK=off go test -run 'TestFallback' ./... 2>&1 | head -20
```

Expected: FAIL to build — `undefined: fallbackRefusal`, `undefined: fallbackReason`, `undefined: fallbackBody`, `undefined: fallbackServer`.

- [ ] **Step 3: Write the minimal implementation**

Add to `cmd/cortex/fallback.go` (extend its imports with `"bufio"`, `"net"`, `"strconv"`, `"sync"`, `"time"`):

```go
// The refusal is fail-closed by design: traffic does not reach the internet unobserved
// just because the proxy is down. What it buys over a refused connection is a client
// that can say why, and that depends on which field the client reads -- measured, not
// assumed:
//
//	curl 8.7.1        "CONNECT tunnel failed, response 502"   neither field
//	Go net/http       the reason phrase, as the error          reason phrase
//	Claude Code       prints it inside "API Error: 502 ..."    body
//
// So both are set. This is also why the response is written as bytes rather than through
// http.Error: Go's net/http always writes the canonical status text for the code and
// offers no way to set a reason phrase, which would silently cost the Go half.
//
// The body is one line with no trailing newline. Claude Code wraps it in framing of its
// own, and a trailing newline landed a stray "." on a line by itself.
const (
	fallbackReason = "Cortex proxy is not serving"
	fallbackBody   = "Cortex is routed in this environment (HTTPS_PROXY) but is not serving — run: agentop service start"
)

// fallbackIOTimeout bounds a connection the fallback is answering. Without it a client
// that opens and never writes would hold a goroutine for as long as the proxy stays
// down, which can be the whole of a long port conflict.
const fallbackIOTimeout = 5 * time.Second

var fallbackRefusal = []byte("HTTP/1.1 502 " + fallbackReason + "\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Length: " + strconv.Itoa(len(fallbackBody)) + "\r\n" +
	"Connection: close\r\n" +
	"Proxy-Connection: close\r\n" +
	"\r\n" + fallbackBody)

// fallbackServer answers every connection with fallbackRefusal. The supervisor holds it
// open only while no child is serving, so in steady state it is closed and a healthy
// install is unchanged: no extra hop, no second process, no new port.
type fallbackServer struct {
	mu sync.Mutex
	ln net.Listener
}

// open binds addr and starts answering. A bind that fails is logged and otherwise
// ignored: restarting the child is the supervisor's actual job, and losing the
// diagnostic listener must never stop it. The usual cause is the previous child not
// having released the port yet, which the next iteration retries anyway.
func (f *fallbackServer) open(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Warn("supervisor: could not open the fallback listener, so a client will see a refused connection rather than an explanation",
			"addr", addr, "error", err)
		return
	}
	f.openOn(ln)
}

// openOn is open's second half, split out so a test can hand in a listener on a port
// the kernel picked instead of racing to bind a port it found free.
func (f *fallbackServer) openOn(ln net.Listener) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln != nil {
		_ = ln.Close()
		return
	}
	f.ln = ln
	slog.Debug("supervisor: fallback listener open", "addr", ln.Addr().String())
	go f.serve(ln)
}

// close frees the address. The supervisor calls this immediately before starting a
// child, which is the only reason the child can bind at all, so a close that leaked the
// port would make every restart a port conflict.
func (f *fallbackServer) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln == nil {
		return
	}
	_ = f.ln.Close()
	f.ln = nil
}

func (f *fallbackServer) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return // closed, which is the normal way this ends
		}
		go refuse(c)
	}
}

// refuse answers one connection and closes it. The request is read as far as its first
// line and then ignored -- the answer does not depend on it -- but it is read rather
// than answered blind so the client has finished writing before the close, which keeps
// the peer from seeing a reset in place of the response it was about to read.
//
// Logging is DEBUG per connection on purpose: clients retry, Claude Code three times
// per request, so anything louder would fill the log while the proxy is down.
func refuse(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(fallbackIOTimeout))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil && line == "" {
		return
	}
	slog.Debug("supervisor: refused a request while the proxy is not serving",
		"request", strings.TrimSpace(line), "remote", c.RemoteAddr().String())
	_, _ = c.Write(fallbackRefusal)
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
cd cmd/cortex && GOWORK=off go test -run 'TestFallback' -v ./... 2>&1 | tail -25
```

Expected: PASS for all four tests. `TestFallbackServer_GoClientSeesTheReasonPhrase` is the one worth reading the output of — it proves the reason phrase survives the Go transport.

- [ ] **Step 5: Commit**

```bash
cd /Users/cwiklik/dev/aiplatform/public/cortex/.worktrees/down-breaks-harness
gofmt -l cmd/cortex
git add cmd/cortex/fallback.go cmd/cortex/fallback_test.go
git -c commit.gpgsign=false commit -s -F - <<'EOF'
feat: Add the fail-closed fallback listener

It answers every connection with a 502 naming Cortex and the command that
fixes it, then closes. Both the reason phrase and the body are set because
neither reaches every client: a Go client reads the reason phrase, Claude
Code reads the body, and curl discards both. That is also why the response
is written as bytes rather than through http.Error, which always writes the
canonical status text.

Refs #1002

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 3: Hold the address while no child is serving

**Files:**
- Modify: `cmd/cortex/supervise.go:162-266` (`runSupervisor`)
- Test: `cmd/cortex/fallback_test.go`

**Interfaces:**
- Consumes: `supervisorForwardAddr(args []string) string` (Task 1); `(*fallbackServer).open`, `.close` (Task 2).
- Produces: nothing new. This is the wiring.

- [ ] **Step 1: Write the failing test**

Append to `cmd/cortex/fallback_test.go`:

```go
// The sequence the supervisor runs, asserted as a property rather than by driving
// runSupervisor, which execs a real binary: the fallback must be closed while a child
// holds the address and open again once it does not. Binding the address in between is
// the stand-in for the child.
func TestFallbackServer_YieldsTheAddressToAChildAndTakesItBack(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	fb := &fallbackServer{}
	fb.openOn(ln)

	// What the supervisor does before every cmd.Start().
	fb.close()
	child, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the child could not bind %s after the fallback closed: %v", addr, err)
	}

	// While the child serves, the fallback must not be listening -- opening it here is
	// what the supervisor must NOT do, and if it did, this bind would fail.
	_ = child.Close()

	// What the supervisor does once the child exits.
	fb.open(addr)
	t.Cleanup(fb.close)

	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("nothing is listening on %s after the child exited: %v", addr, err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("CONNECT x:443 HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(c)
	if !strings.Contains(string(b), fallbackReason) {
		t.Errorf("after the child exited the address answers %q, want the refusal", string(b))
	}
}
```

- [ ] **Step 2: Run the test to verify it passes already**

```bash
cd cmd/cortex && GOWORK=off go test -run 'TestFallbackServer_YieldsTheAddress' -v ./... 2>&1 | tail -10
```

Expected: PASS. This test covers the contract Task 2 already satisfies; it is written here because it is the property Step 3's wiring depends on, and it would fail if a later change made `close` lazy or `open` asynchronous. Step 3's own change has no unit test — it is six lines of sequencing inside a function that execs a real binary — so this test plus the manual check in Step 5 is the coverage.

- [ ] **Step 3: Wire it into `runSupervisor`**

In `cmd/cortex/supervise.go`, after the argument-filtering guard (currently ending at line 177 with the `internal: supervise flag survived argument filtering` check) and before the `stopping` channel is set up, add:

```go
	// The fallback holds the forward address whenever no child does, so a routed agent
	// gets a 502 that names Cortex instead of a refused connection it cannot trace. It
	// is opened here because nothing is serving yet, and closed before every start. See
	// fallback.go. It deliberately does not cover `agentop service stop`: that stops
	// this supervisor too, and that path already warns, having been asked for.
	fallback := &fallbackServer{}
	fallbackAddr := supervisorForwardAddr(args)
	fallback.open(fallbackAddr)
	defer fallback.close()
```

Inside the `for {` loop, make the first statement a close, immediately before `cmd := exec.Command(...)`:

```go
	for {
		// Hand the address to the child. Nothing else frees it, so this is what lets the
		// child bind at all.
		fallback.close()

		cmd := exec.Command(self, args...) //nolint:gosec // our own binary, filtered args
```

And after the `wait` loop ends — directly after the closing `}` of `wait:` and before `note := policy.exited(...)` — take the address back:

```go
		}

		// The child is gone, so the address is free and nothing would answer on it.
		fallback.open(fallbackAddr)

		note := policy.exited(portInUseExit(werr), wasHealthy)
```

- [ ] **Step 4: Run the full module test suite**

```bash
cd cmd/cortex && GOWORK=off go vet ./... && GOWORK=off go test ./... 2>&1 | tail -15
```

Expected: `vet` silent, every package `ok`. Pay attention to `cmd/cortex`'s existing supervisor and port-conflict tests — `TestSecondInstanceExitsBeforeOpeningData` and the `restartPolicy` tests must still pass. If a port-conflict test now fails, the likely cause is the fallback holding the address when the test expected it free; that is a real bug in the wiring, not a bad test.

- [ ] **Step 5: Verify by hand against a real client**

This is the behaviour the whole change exists for and no unit test covers it end to end. Build the binary and run the supervisor against a config whose proxy cannot start, then confirm a Go client gets the reason phrase:

```bash
cd /Users/cwiklik/dev/aiplatform/public/cortex/.worktrees/down-breaks-harness/cmd/cortex
GOWORK=off go build -o /tmp/cortex-1002 .
# A config that will not load is the headline case: the fallback must still bind the
# default address and answer.
/tmp/cortex-1002 --supervise --config /nonexistent/cortex.yaml &
sleep 2
curl -sS -x http://127.0.0.1:47600 https://example.com 2>&1 | head -3
kill %1
```

Expected: `curl: (56) CONNECT tunnel failed, response 502`. **Do this only if nothing else is using `127.0.0.1:47600`** — check with `lsof -nP -iTCP:47600 -sTCP:LISTEN` first, and if the real Cortex is running there, skip this step rather than stopping it, because a running Cortex is shared with every other session on this machine.

- [ ] **Step 6: Commit**

```bash
cd /Users/cwiklik/dev/aiplatform/public/cortex/.worktrees/down-breaks-harness
gofmt -l cmd/cortex
git add cmd/cortex/supervise.go cmd/cortex/fallback_test.go
git -c commit.gpgsign=false commit -s -F - <<'EOF'
feat: Hold the forward address while no proxy child is serving

The supervisor opens the fallback listener at startup and after every child
exit, and closes it immediately before starting one. In steady state it is
closed, so a healthy install is unchanged. This covers the cases nobody ran
a command for -- a child crash-looping, a config that will not load, a
killed child, and the long backoff a held port causes.

Refs #1002

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 4: Document it, then verify the whole module

The message is user-facing and someone meeting it will search for its text, so it needs to be findable. The spec did not call for a doc change; this task adds one because `docs/laptop-service.md` is where the service's user-facing behaviour is described and the new message belongs there.

**Files:**
- Modify: `docs/laptop-service.md` (insert before the `### Everything shows as `tunnel` and no plugin ever runs` heading, currently line 542)

- [ ] **Step 1: Add the section**

Insert immediately before the line `### Everything shows as \`tunnel\` and no plugin ever runs`:

```markdown
### `502 Cortex proxy is not serving`

An agent routed through Cortex shows this when the proxy is installed and routed but
not currently serving — a config it cannot load, a crash it is restarting from, or an
address another process holds. The supervisor answers on the proxy's address while no
proxy is running, so that you get this message instead of a bare connection error that
names nothing:

```
API Error: 502 Cortex is routed in this environment (HTTPS_PROXY) but is not
serving — run: agentop service start
```

The fix is the one in the message. If `agentop service start` does not take, run
`agentop doctor`, which names the reason, and read `~/.cortex/proxy.log` for what the
proxy said as it failed.

Two things to expect. Requests **fail** rather than going out unobserved, which is
deliberate: Cortex does not let traffic past unseen just because it is down. And the
message takes a while to appear — Claude Code retries a refused request a few times
before reporting, so allow a couple of minutes. A client that shows nothing useful is
most likely `curl`, which discards the explanation and reports only
`CONNECT tunnel failed, response 502`.

This does **not** appear after `agentop service stop`, which stops the supervisor too.
That path tells you what it cut when you run it.
```

- [ ] **Step 2: Verify the whole module, every gate CI applies**

```bash
cd /Users/cwiklik/dev/aiplatform/public/cortex/.worktrees/down-breaks-harness
gofmt -l cmd/cortex                                     # must print nothing
cd cmd/cortex && GOWORK=off go vet ./... && GOWORK=off go test ./... 2>&1 | tail -15
GOWORK=off go mod tidy -diff                            # must print nothing
```

Expected: `gofmt` silent, `vet` silent, every package `ok`, `tidy -diff` silent. `go mod tidy -diff` is checked because `ci.yaml`'s `go-tidy-check` gates on it and `build`/`vet`/`test` all pass while it fails — though this change adds no dependency, so it should be clean.

- [ ] **Step 3: Commit**

```bash
cd /Users/cwiklik/dev/aiplatform/public/cortex/.worktrees/down-breaks-harness
git add docs/laptop-service.md
git -c commit.gpgsign=false commit -s -F - <<'EOF'
docs: Document the fail-closed refusal when the proxy is not serving

Someone meeting the message will search for its text, so it needs to be
findable. Records the two things that surprise: requests fail rather than
going out unobserved, and Claude Code retries before reporting, so the
message takes a couple of minutes to appear.

Refs #1002

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

- [ ] **Step 4: Open the PR**

```bash
cd /Users/cwiklik/dev/aiplatform/public/cortex/.worktrees/down-breaks-harness
git push -u origin fix/down-breaks-harness
```

Then open it with a **capitalized** prefix — the CI title check is case-sensitive and rejects lowercase:

```bash
gh pr create --title 'Fix: Refuse legibly when Cortex is routed but not serving' --body "$(cat <<'EOF'
## Summary

Cortex's `HTTPS_PROXY` routing outlives the proxy, so when the proxy is not serving a
routed agent got a bare connection-refused that named nothing. The supervisor now holds
the forward address whenever no child is serving and refuses fail-closed with a 502 that
names Cortex and the command that fixes it.

In steady state the fallback listener is closed, so a healthy install is unchanged — no
extra hop, no second service, no new port, no config surface.

## What this covers, and what it does not

The explicit-`stop` path already warned and `uninstall` already unrouted, so this targets
what was left: a child crash-looping, a config that will not load, a killed child, and the
long backoff a held address causes. It deliberately does not cover `agentop service stop`
(which stops the supervisor too, and already warns) or a unit that never loads (`doctor`).

## Why both the reason phrase and the body

No single response field reaches every client. Measured against real ones:

| Client | What the user sees |
|---|---|
| `curl` 8.7.1 | `CONNECT tunnel failed, response 502` — discards both |
| Go `net/http` | the reason phrase, as the error |
| Claude Code | the body, inside `API Error: 502 …` |

So both are set, and the response is written on the raw socket: Go's `net/http` always
writes the canonical status text and offers no way to set a reason phrase, which would
have cost the Go half silently.

Fail-closed buys legibility, not speed — Claude Code retries a refused request a few
times before reporting, so the message takes a couple of minutes to appear. That is
documented rather than papered over.

Design: `docs/superpowers/specs/2026-10-13-supervisor-fallback-listener-design.md`

Fixes #1002

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
)"
```

- [ ] **Step 5: Confirm CI actually ran**

```bash
gh pr checks --watch 2>&1 | tail -20
gh pr view --json mergeable,mergeStateStatus --jq '{mergeable, mergeStateStatus}'
```

`mergeable` must be `MERGEABLE`. A conflicting PR runs almost no CI and looks greener than a failing one, so read `mergeable` rather than trusting the check list. If `Go CI (core)` fails with `dropped 96, want 95`, that is a known ~2.3% race in `core/session/archive` unrelated to this change — re-run that job.

---

## Self-Review

**Spec coverage.** Every section of the spec maps to a task: the behaviour and the open/close sequence to Task 3; the response bytes and both load-bearing fields to Task 2; the address resolver and its fallback to Task 1; each listed test to a named test in Tasks 1–3. Of the spec's four error-handling items, three are implemented and tested — the ignored bind failure (Task 2 `open`), the connection storm (DEBUG logging and `fallbackIOTimeout`), and the close-then-start race (Task 3's ordering, asserted by `TestFallbackServer_YieldsTheAddressToAChildAndTakesItBack`). The fourth, self-attribution, is a constraint on future code rather than behaviour to build: the fallback is closed before the child starts, so nothing probes a port it holds. No task is needed and none is added.

**One addition beyond the spec:** Task 4's doc change. The spec listed no documentation work, which is a gap — the message is user-facing and searchable, and `docs/laptop-service.md` is where this service's behaviour is described.

**Placeholder scan.** No TBD/TODO, no "add error handling", no "similar to Task N". Every code step carries the actual code. Task 2 Step 1 contains one deliberate correction-in-place (the first `TestFallbackServer_CloseFreesTheAddress` is superseded by the second, with an instruction to use the second and delete the first) — kept because the reason the first is wrong is instructive, and the instruction is explicit.

**Type consistency.** `fallbackServer`, `open`, `openOn`, `close`, `serve`, `refuse`, `fallbackRefusal`, `fallbackReason`, `fallbackBody`, `fallbackIOTimeout`, `fallbackDefaultAddr`, `configPathFromArgs`, `supervisorForwardAddr` are spelled identically in every task that uses them. `supervisorForwardAddr` takes and `configPathFromArgs` takes `[]string` in both their definition and Task 3's call. The config field is `cfg.Listener.ForwardProxyAddr` throughout.
