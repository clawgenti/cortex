package forwardproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/memstore"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter"
	"github.com/rossoctl/cortex/core/session"
)

// seen is every request an origin received, by method and Authorization header —
// HEADs included, unlike origin.gets, so a key on any request is caught.
type seen struct {
	mu    sync.Mutex
	calls []string
}

func (s *seen) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls = append(s.calls, r.Method+" "+r.Header.Get("Authorization"))
		s.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (s *seen) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// recordingOrigin is newOrigin with every request's method and Authorization kept.
func recordingOrigin(t *testing.T, name string) (*origin, *seen) {
	t.Helper()
	s := &seen{}
	o := newOrigin(t, name, func(srv *httptest.Server) {
		srv.Config.Handler = s.record(srv.Config.Handler)
		srv.StartTLS()
	})
	return o, s
}

// A client that CONNECTs to one host and names an inference server's host inside the
// bridged TLS must not get that server's key sent to the host it CONNECTed to. The
// router decides on the Host header, which on a bridged request is the client's word,
// while the forward proxy dials the CONNECT authority unless a redirect says
// otherwise. The listener now refuses such a request with a 421 before any plugin
// runs, so the key goes nowhere; the router's own redirect is the second line, which
// TestConnectBridge_RedirectSendsTheDecryptedRequestToTheTarget still covers.
func TestConnectBridge_TheRoutersKeyGoesOnlyToItsServer(t *testing.T) {
	const key = "ete-secret-key"
	ete, eteSeen := recordingOrigin(t, "FROM-ETE")
	attacker, attackerSeen := recordingOrigin(t, "FROM-ATTACKER")

	router := inferencerouter.New()
	if err := router.Configure(json.RawMessage(`{
		"servers": {"ete": {"url": "` + ete.URL + `", "key": "` + key + `", "main": "opus", "helper": "haiku"}},
		"agents": {"claude-code": "ete"}
	}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p, err := pipeline.New([]pipeline.Plugin{router})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	// The bridge intercepts the attacker's port, the one the client CONNECTs to.
	// httptest gives every TLS server one certificate, so trusting the attacker's
	// trusts ete's: the bridge would complete either handshake, and only the dial
	// target decides who gets the request.
	engine := redirectBridge(t, portOf(attacker.authority()), certPEM(attacker.Server))
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	shared := memstore.New()
	defer shared.Close()
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Shared:           shared,
		Client:           http.DefaultClient,
		TLSBridge:        engine,
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	status, body := bridgedGet(t, proxy, engine.CAPEM, attacker.authority(), "/v1/messages", func(r *http.Request) {
		r.Host = ete.authority()
		r.Header.Set("User-Agent", "claude-cli/2.1.0 (external, cli)")
		r.Header.Set("Authorization", "Bearer client-key")
	})

	for _, call := range attackerSeen.all() {
		if call != http.MethodHead+" " {
			t.Errorf("the CONNECT authority received %q; only the bridge's keyless HEAD probe may reach it", call)
		}
	}
	if hosts, _ := attacker.gets(); len(hosts) != 0 {
		t.Errorf("the CONNECT authority served the request (hosts %v): the router's key went where the client CONNECTed", hosts)
	}
	if status != http.StatusMisdirectedRequest {
		t.Errorf("response = %d %q, want 421: the Host names another server than the tunnel's", status, body)
	}
	for _, call := range eteSeen.all() {
		if call != http.MethodHead+" " {
			t.Errorf("ete saw %q; a misdirected request must reach no server", call)
		}
	}
}

// keyOrigin is a plain-http origin that records the Authorization of every request
// it serves, and answers with its name.
func keyOrigin(t *testing.T, name string) (*httptest.Server, *seen) {
	t.Helper()
	s := &seen{}
	srv := httptest.NewServer(s.record(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(name))
	})))
	t.Cleanup(srv.Close)
	return srv, s
}

// The documented first setup, through the listener: a Claude Code session is
// already talking to ete when the router is first added and claude-code routed to
// glm, and sends nothing in between. The router has never seen that session, so it
// holds no pin; the session store's history does, and the next turn stays on ete,
// with ete's key, rather than moving to glm mid-conversation. A session that starts
// afterwards goes to glm. Both servers are plain http on loopback, which the router
// sends to without a TLS bridge; 127.0.0.1 and localhost are two hosts to it.
func TestForwardProxy_ASessionRunningBeforeRoutingWasConfiguredStaysOnItsServer(t *testing.T) {
	ete, eteSeen := keyOrigin(t, "FROM-ETE")
	glm, glmSeen := keyOrigin(t, "FROM-GLM")
	eteURL := ete.URL
	glmURL := strings.Replace(glm.URL, "127.0.0.1", "localhost", 1)

	parserOnly, err := pipeline.New([]pipeline.Plugin{inferenceparser.NewInferenceParser()})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	holder := pipeline.NewHolder(parserOnly)
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	shared := memstore.New()
	defer shared.Close()
	srv := &Server{
		OutboundPipeline: holder,
		Sessions:         store,
		Shared:           shared,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
		Client:           http.DefaultClient,
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	turn := func(sessionID string) string {
		t.Helper()
		body := `{"model":"claude-opus-5-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
		req, err := http.NewRequest(http.MethodPost, eteURL+"/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "claude-cli/2.1.286 (external, cli)")
		req.Header.Set("Authorization", "Bearer client-key")
		req.Header.Set(session.ClaudeCodeSessionHeader, sessionID)
		resp, err := proxyClient(proxy, nil).Do(req)
		if err != nil {
			t.Fatalf("request through the proxy: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	// Before the router: the session talks to ete with its own key.
	if got := turn("running"); got != "FROM-ETE" {
		t.Fatalf("first turn reached %q, want ete", got)
	}

	// agentop server add ete, add glm, use glm --agent claude-code: one reload.
	router := inferencerouter.New()
	if err := router.Configure(json.RawMessage(`{
		"servers": {"ete": {"url": "` + eteURL + `", "key": "ete-key", "main": "opus", "helper": "haiku"},
			"glm": {"url": "` + glmURL + `", "key": "glm-key", "main": "glm", "helper": "nemotron"}},
		"agents": {"claude-code": "glm"}
	}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	routed, err := pipeline.New([]pipeline.Plugin{inferenceparser.NewInferenceParser(), router})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	holder.Store(routed)

	if got := turn("running"); got != "FROM-ETE" {
		t.Errorf("the running session's next turn reached %q, want ete: it moved mid-conversation", got)
	}
	if got := turn("fresh"); got != "FROM-GLM" {
		t.Errorf("a new session reached %q, want glm", got)
	}
	if got, want := eteSeen.all(), []string{"POST Bearer client-key", "POST Bearer ete-key"}; !slices.Equal(got, want) {
		t.Errorf("ete saw %q, want %q: the running session keeps its server and gets that server's key", got, want)
	}
	if got, want := glmSeen.all(), []string{"POST Bearer glm-key"}; !slices.Equal(got, want) {
		t.Errorf("glm saw %q, want only the new session's %q", got, want)
	}
}

// moveOriginSrv is an origin that records each request's body, so a test can
// assert on what the destination received.
type moveOriginSrv struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func moveOrigin(t *testing.T, name string) *moveOriginSrv {
	t.Helper()
	o := &moveOriginSrv{}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		o.mu.Lock()
		o.bodies = append(o.bodies, string(b))
		o.mu.Unlock()
		_, _ = w.Write([]byte(name))
	}))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *moveOriginSrv) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.bodies)
}

func (o *moveOriginSrv) lastBody() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.bodies) == 0 {
		return ""
	}
	return o.bodies[len(o.bodies)-1]
}

// A session moved by a sessions: override lands on the new server with the new
// server's key, the old server's thinking dropped and the new server's kept, one
// request per turn, and stays there once the override is gone — the pin the move
// settled. A move to its own provider gets back the thinking that provider wrote.
func TestForwardProxy_ASessionsOverrideMovesARunningSession(t *testing.T) {
	ete := moveOrigin(t, "FROM-ETE")
	glm := moveOrigin(t, "FROM-GLM")
	eteURL := ete.srv.URL
	glmURL := strings.Replace(glm.srv.URL, "127.0.0.1", "localhost", 1)

	router := inferencerouter.New()
	parserOnly, err := pipeline.New([]pipeline.Plugin{inferenceparser.NewInferenceParser()})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	holder := pipeline.NewHolder(parserOnly)
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	shared := memstore.New()
	defer shared.Close()
	srv := &Server{
		OutboundPipeline: holder,
		Sessions:         store,
		Shared:           shared,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
		Client:           http.DefaultClient,
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// Each turn's body is the conversation so far: the client appends every
	// answer, its thinking block included, and re-sends the whole history.
	var turns []string
	turn := func(sessionID, answerSig string) string {
		t.Helper()
		msgs := []string{`{"role":"user","content":"hi"}`}
		for _, sig := range turns {
			msgs = append(msgs, `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"`+sig+`"},{"type":"text","text":"hello"}]}`,
				`{"role":"user","content":"again"}`)
		}
		body := `{"model":"claude-opus-5-5","max_tokens":8,"messages":[` + strings.Join(msgs, ",") + `]}`
		req, err := http.NewRequest(http.MethodPost, eteURL+"/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "claude-cli/2.1.286 (external, cli)")
		req.Header.Set("Authorization", "Bearer client-key")
		req.Header.Set(session.ClaudeCodeSessionHeader, sessionID)
		resp, err := proxyClient(proxy, nil).Do(req)
		if err != nil {
			t.Fatalf("request through the proxy: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		turns = append(turns, answerSig)
		return string(b)
	}
	configure := func(sessions string) {
		t.Helper()
		if err := router.Configure(json.RawMessage(`{
			"servers": {"ete": {"url": "` + eteURL + `", "key": "ete-key", "main": "opus", "helper": "haiku"},
				"glm": {"url": "` + glmURL + `", "key": "glm-key", "main": "glm", "helper": "nemotron"}},
			"agents": {"claude-code": "ete"},
			"sessions": {` + sessions + `}
		}`)); err != nil {
			t.Fatalf("Configure: %v", err)
		}
	}
	signatures := func(body string) []string {
		var out []string
		for _, part := range strings.Split(body, `"signature":"`)[1:] {
			out = append(out, part[:strings.Index(part, `"`)])
		}
		return out
	}

	// The session runs on ete for two turns, and ete gets its own thinking back.
	configure(``)
	routed, err := pipeline.New([]pipeline.Plugin{inferenceparser.NewInferenceParser(), router})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	holder.Store(routed)
	turn("running", "e1")
	if got := turn("running", "e2"); got != "FROM-ETE" {
		t.Fatalf("turn reached %q, want ete", got)
	}
	if got := signatures(ete.lastBody()); !slices.Equal(got, []string{"e1"}) {
		t.Errorf("ete received %v, want its own e1", got)
	}

	// agentop server move to glm: one request, ete's thinking dropped before it is
	// sent — only ete can read it — and the assistant text kept.
	configure(`"running": "glm"`)
	if got := turn("running", "g1"); got != "FROM-GLM" {
		t.Fatalf("moved turn reached %q, want glm", got)
	}
	if body := glm.lastBody(); len(signatures(body)) != 0 || !strings.Contains(body, `"hello"`) {
		t.Errorf("glm received %s, want ete's thinking gone and the text kept", body)
	}

	// The override gone, the session stays on glm, and glm keeps its own thinking.
	configure(``)
	if got := turn("running", "g2"); got != "FROM-GLM" {
		t.Fatalf("post-override turn reached %q, want glm: the pin must follow the move", got)
	}
	if got := signatures(glm.lastBody()); !slices.Equal(got, []string{"g1"}) {
		t.Errorf("glm received %v, want only its own g1", got)
	}
	if n := glm.count(); n != 2 {
		t.Errorf("glm received %d requests, want one per turn", n)
	}

	// own: the entry STAYS as "" — removing it would let the pin route the session
	// right back — and the request goes where the client sent it, ete, which gets
	// back the thinking it wrote and none of glm's.
	configure(`"running": ""`)
	if got := turn("running", "o1"); got != "FROM-ETE" {
		t.Fatalf("own turn reached %q, want ete: the empty entry must stop the routing", got)
	}
	if got := signatures(ete.lastBody()); !slices.Equal(got, []string{"e1", "e2"}) {
		t.Errorf("ete's own turn received %v, want its own e1 and e2", got)
	}
}
