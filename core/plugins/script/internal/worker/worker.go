// Package worker runs one script as a long-lived child process and exchanges
// line-delimited JSON with it: one object per line on the child's stdin, one per
// line back on its stdout. It knows nothing about HTTP or the pipeline; the
// script plugin builds the messages and reads the replies.
//
// The protocol:
//
//   - The child's first line is its handshake (see Handshake). Nothing is sent
//     until it arrives.
//   - Each Call writes one object carrying an "id" and reads lines until one is
//     an object with that id. Every other line is the script's own output — a
//     print, a library's warning — and is returned with the reply as Logs, so
//     stray output cannot corrupt the exchange.
//   - Calls are serialised: a script is a plain read-reply loop, and its state
//     cannot race. A call's deadline covers its wait for the calls ahead of it,
//     so a slow script delays requests by at most the call timeout, never by the
//     length of a queue.
//   - A call that overruns its deadline kills the child's whole process group,
//     because a reply arriving later could no longer be matched to its request.
//     So does a child that exits. Either way a new one is started in the
//     background after a backoff, and the script's in-memory state is gone. A call
//     never waits for a start: one that finds no running child fails at once
//     (ErrStarting), so a slow-starting script costs requests nothing.
package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"time"
)

// Protocol is the handshake version this package speaks.
const Protocol = 1

// Defaults for a Config that leaves them zero.
const (
	DefaultStartTimeout = 5 * time.Second
	DefaultCallTimeout  = 2 * time.Second
	minBackoff          = 100 * time.Millisecond
	maxBackoff          = 30 * time.Second
	stopGrace           = time.Second
	// maxLine bounds one line from the child. A reply can carry a rewritten
	// request body, and the forward proxy admits bodies up to 32MiB, so this sits
	// above that with room for JSON escaping.
	maxLine = 96 << 20
	// maxLogs bounds what one call keeps of the script's own output.
	maxLogs     = 50
	maxLogBytes = 2048
)

// ErrBackoff is returned by Call while a crashed worker waits to be restarted.
var ErrBackoff = errors.New("worker: restarting after a failure")

// ErrStarting is returned by Call while the child is being started in the
// background.
var ErrStarting = errors.New("worker: script is starting")

// Config describes the child to run.
type Config struct {
	// Path is the executable: the script itself, run through its shebang.
	Path string
	// Args are passed after Path. Empty for a script.
	Args []string
	// Env is the child's whole environment. See Environ.
	Env []string
	// StartTimeout bounds the wait for the handshake.
	StartTimeout time.Duration
	// CallTimeout bounds one Call when its context carries no earlier deadline.
	CallTimeout time.Duration
}

// Handshake is the child's first line.
type Handshake struct {
	// Cortex is the protocol version the child speaks; it must be Protocol.
	Cortex int `json:"cortex"`
	// Hooks names the hooks the script implements: "request", "response".
	Hooks []string `json:"hooks"`
	// Enforce: the script's denies and edits apply. False is shadow mode.
	Enforce bool `json:"enforce"`
	// RewriteResponse: the script may rewrite response bodies.
	RewriteResponse bool `json:"rewrite_response"`
}

// Has reports whether the script implements hook.
func (h Handshake) Has(hook string) bool { return slices.Contains(h.Hooks, hook) }

// Reply is what one Call returned.
type Reply struct {
	// Line is the reply object, id included.
	Line json.RawMessage
	// Logs are the lines the script wrote that were not the reply, oldest first.
	Logs []string
}

// Worker is one script's child process, restarted as needed. Safe for concurrent
// use; calls are serialised.
type Worker struct {
	cfg  Config
	name string

	// sem serialises everything that touches the child. A channel rather than a
	// mutex so that waiting for it can give up when a call's deadline passes.
	sem       chan struct{}
	proc      *process
	handshake Handshake
	nextID    uint64
	failures  int
	retryAt   time.Time
	stopped   bool
	starting  bool
	now       func() time.Time
}

// New returns a worker for cfg. Nothing runs until Start or the first Call.
func New(cfg Config) *Worker {
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = DefaultStartTimeout
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = DefaultCallTimeout
	}
	return &Worker{cfg: cfg, name: filepath.Base(cfg.Path), sem: make(chan struct{}, 1), now: time.Now}
}

func (w *Worker) lock(ctx context.Context) error {
	select {
	case w.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("worker: %s is busy with an earlier call: %w", w.name, ctx.Err())
	}
}

func (w *Worker) unlock() { <-w.sem }

// Start runs the child and waits for its handshake. Calling it on a running
// worker returns the running child's handshake.
func (w *Worker) Start(ctx context.Context) (Handshake, error) {
	if err := w.lock(ctx); err != nil {
		return Handshake{}, err
	}
	defer w.unlock()
	if err := w.ensure(ctx); err != nil {
		return Handshake{}, err
	}
	return w.handshake, nil
}

// Handshake returns the last handshake the child sent; zero before the first start.
func (w *Worker) Handshake() Handshake {
	w.sem <- struct{}{}
	defer w.unlock()
	return w.handshake
}

// Call sends msg with a fresh "id" and returns the child's reply to it. msg must
// marshal to a JSON object.
func (w *Worker) Call(ctx context.Context, msg map[string]any) (Reply, error) {
	ctx, cancel := context.WithTimeout(ctx, w.cfg.CallTimeout)
	defer cancel()
	if err := w.lock(ctx); err != nil {
		return Reply{}, err
	}
	defer w.unlock()
	if err := w.ready(); err != nil {
		return Reply{}, err
	}
	w.nextID++
	id := w.nextID
	msg["id"] = id
	line, err := json.Marshal(msg)
	delete(msg, "id")
	if err != nil {
		return Reply{}, fmt.Errorf("worker: encode: %w", err)
	}
	reply, err := w.proc.call(ctx, append(line, '\n'), id)
	if err != nil {
		w.fail(err)
		return reply, err
	}
	w.failures = 0
	return reply, nil
}

// Stop ends the child and refuses further calls.
func (w *Worker) Stop() {
	w.sem <- struct{}{}
	defer w.unlock()
	w.stopped = true
	if w.proc != nil {
		w.proc.stop()
		w.proc = nil
	}
}

// ready reports whether a child is running to take a call, and when none is, starts
// one in the background rather than making this call wait for it. Caller holds sem.
func (w *Worker) ready() error {
	if w.stopped {
		return errors.New("worker: stopped")
	}
	if w.proc != nil && !w.proc.exited() {
		return nil
	}
	if w.proc != nil {
		w.fail(fmt.Errorf("worker: %s exited: %v", w.name, w.proc.exitErr()))
	}
	if w.now().Before(w.retryAt) {
		return ErrBackoff
	}
	if !w.starting {
		w.starting = true
		go func() {
			w.sem <- struct{}{}
			defer w.unlock()
			w.starting = false
			_ = w.ensure(context.Background())
		}()
	}
	return ErrStarting
}

// ensure starts the child when none is running. Caller holds sem. The start is
// bounded by StartTimeout and by ctx, whichever ends first.
func (w *Worker) ensure(ctx context.Context) error {
	if w.stopped {
		return errors.New("worker: stopped")
	}
	if w.proc != nil && !w.proc.exited() {
		return nil
	}
	if w.proc != nil {
		w.fail(fmt.Errorf("worker: %s exited: %v", w.name, w.proc.exitErr()))
	}
	if w.now().Before(w.retryAt) {
		return ErrBackoff
	}
	ctx, cancel := context.WithTimeout(ctx, w.cfg.StartTimeout)
	defer cancel()
	p, hs, err := spawn(ctx, w.cfg, w.name)
	if err != nil {
		w.fail(err)
		return err
	}
	w.proc, w.handshake = p, hs
	return nil
}

// fail kills the child, if any, and schedules the next start. Caller holds sem.
// Killed rather than asked to stop: a child that overran a call may still write
// that call's reply, and nothing could match it to a request any more.
func (w *Worker) fail(err error) {
	if w.proc != nil {
		w.proc.kill()
		w.proc = nil
	}
	backoff := minBackoff << min(w.failures, 10)
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	w.failures++
	w.retryAt = w.now().Add(backoff)
	slog.Warn("script worker failed; restarting on a later call", "script", w.name, "error", err, "retryIn", backoff)
}

// process is one run of the child.
type process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Reader
	done   chan struct{}
	waited error
}

func spawn(ctx context.Context, cfg Config, name string) (*process, Handshake, error) {
	cmd := exec.Command(cfg.Path, cfg.Args...)
	cmd.Dir = filepath.Dir(cfg.Path)
	cmd.Env = cfg.Env
	setProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, Handshake{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, Handshake{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, Handshake{}, err
	}
	if err := cmd.Start(); err != nil {
		return nil, Handshake{}, fmt.Errorf("worker: start %s: %w", name, err)
	}
	p := &process{cmd: cmd, stdin: stdin, out: bufio.NewReaderSize(stdout, 64<<10), done: make(chan struct{})}
	go logStderr(stderr, name)
	go func() { p.waited = cmd.Wait(); close(p.done) }()

	var hs Handshake
	var logs []string
	for {
		line, err := p.readLine(ctx)
		if err != nil {
			p.kill()
			return nil, Handshake{}, fmt.Errorf("worker: %s sent no handshake: %w%s", name, err, tail(logs))
		}
		if json.Unmarshal(line, &hs) == nil && hs.Cortex != 0 {
			break
		}
		logs = appendLog(logs, line)
	}
	if hs.Cortex != Protocol {
		p.kill()
		return nil, Handshake{}, fmt.Errorf("worker: %s speaks protocol %d, want %d", name, hs.Cortex, Protocol)
	}
	return p, hs, nil
}

func (p *process) call(ctx context.Context, line []byte, id uint64) (Reply, error) {
	var reply Reply
	if _, err := p.stdin.Write(line); err != nil {
		return reply, fmt.Errorf("worker: write: %w", err)
	}
	for {
		got, err := p.readLine(ctx)
		if err != nil {
			return reply, err
		}
		var head struct {
			ID *uint64 `json:"id"`
		}
		if json.Unmarshal(got, &head) == nil && head.ID != nil && *head.ID == id {
			reply.Line = got
			return reply, nil
		}
		reply.Logs = appendLog(reply.Logs, got)
	}
}

// readLine returns the next line without its newline, or ctx's error. A read that
// outlives ctx is abandoned to its goroutine, still holding p.out. That is safe only
// because every error from here ends this process — Call and spawn both kill it —
// so no later readLine ever shares p.out with an abandoned one. Keep it that way: a
// caller that retried on the same process after an error would race two readers.
func (p *process) readLine(ctx context.Context) ([]byte, error) {
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		var buf []byte
		for {
			chunk, err := p.out.ReadSlice('\n')
			buf = append(buf, chunk...)
			if len(buf) > maxLine {
				ch <- result{err: fmt.Errorf("worker: line longer than %d bytes", maxLine)}
				return
			}
			if err == bufio.ErrBufferFull {
				continue
			}
			ch <- result{bytes.TrimRight(buf, "\r\n"), err}
			return
		}
	}()
	select {
	case r := <-ch:
		if r.err != nil && (r.err != io.EOF || len(r.line) == 0) {
			if r.err == io.EOF {
				return nil, errors.New("worker: script exited")
			}
			return nil, r.err
		}
		return r.line, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("worker: no reply in time: %w", ctx.Err())
	}
}

func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *process) exitErr() error {
	<-p.done
	return p.waited
}

// stop closes stdin so a well-behaved script ends its loop, then kills the whole
// process group if it has not exited within stopGrace.
func (p *process) stop() {
	_ = p.stdin.Close()
	select {
	case <-p.done:
		return
	case <-time.After(stopGrace):
	}
	p.kill()
}

func (p *process) kill() {
	_ = p.stdin.Close()
	killProcessGroup(p.cmd)
	<-p.done
}

func logStderr(r io.Reader, name string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		slog.Info("script stderr", "script", name, "line", truncate(sc.Text()))
	}
}

func appendLog(logs []string, line []byte) []string {
	if len(logs) >= maxLogs {
		return logs
	}
	return append(logs, truncate(string(line)))
}

func truncate(s string) string {
	if len(s) > maxLogBytes {
		return s[:maxLogBytes] + "…"
	}
	return s
}

func tail(logs []string) string {
	if len(logs) == 0 {
		return ""
	}
	return "; it wrote: " + logs[len(logs)-1]
}
