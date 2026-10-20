package discoveryexchange

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/memstore"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/placeholder"
)

// fakeKeycloak answers the token endpoint the way Keycloak 26.5's standard
// token exchange did in a live probe, for a caller whose audience scopes are
// all default scopes:
//
//   - an audience in granted gets a token narrowed to it
//   - an audience in existing but not granted: 400 invalid_request
//   - any other audience: 400 invalid_client "Audience not found"
//   - client_credentials ignores audience and returns every granted audience
type fakeKeycloak struct {
	granted  map[string]bool
	existing map[string]bool
	calls    atomic.Int64

	mu       sync.Mutex
	override func(w http.ResponseWriter, form map[string]string) bool
}

func newFakeKeycloak(t *testing.T) (*fakeKeycloak, *httptest.Server) {
	kc := &fakeKeycloak{
		granted:  map[string]bool{"github-tool": true, "weather-agent": true},
		existing: map[string]bool{"github-tool": true, "weather-agent": true, "hr-tool": true},
	}
	srv := httptest.NewServer(kc)
	t.Cleanup(srv.Close)
	return kc, srv
}

func (kc *fakeKeycloak) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	kc.calls.Add(1)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	form := map[string]string{}
	for k := range r.PostForm {
		form[k] = r.PostForm.Get(k)
	}
	kc.mu.Lock()
	override := kc.override
	kc.mu.Unlock()
	if override != nil && override(w, form) {
		return
	}
	if form["client_id"] != "agent" || form["client_secret"] != "s3cret" {
		oauthError(w, 401, "invalid_client", "Invalid client or Invalid client credentials")
		return
	}
	switch form["grant_type"] {
	case "client_credentials":
		var all []string
		for a := range kc.granted {
			all = append(all, a)
		}
		issue(w, append(all, "account"))
	case grantTokenExchange:
		aud := form["audience"]
		switch {
		case aud == "":
			issue(w, []string{"github-tool", "weather-agent", "account"})
		case kc.granted[aud]:
			issueClaims(w, map[string]any{"aud": []string{aud}, "sub": "alice", "scope": "openid github-read"})
		case kc.existing[aud]:
			oauthError(w, 400, "invalid_request", "Requested audience not available: "+aud)
		default:
			oauthError(w, 400, "invalid_client", "Audience not found")
		}
	default:
		oauthError(w, 400, "unsupported_grant_type", "")
	}
}

func issue(w http.ResponseWriter, aud []string) {
	issueClaims(w, map[string]any{"aud": aud})
}

func issueClaims(w http.ResponseWriter, claims map[string]any) {
	payload, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	tok := enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + ".sig"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "expires_in": 300})
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

func configured(t *testing.T, srv *httptest.Server, extra string) *DiscoveryExchange {
	t.Helper()
	p := New()
	cfg := fmt.Sprintf(`{"token_url":%q,"client_id":"agent","client_secret":"s3cret"%s}`, srv.URL, extra)
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return p
}

func outbound(host, authz string) *pipeline.Context {
	h := http.Header{}
	if authz != "" {
		h.Set("Authorization", authz)
	}
	return &pipeline.Context{Direction: pipeline.Outbound, Host: host, Headers: h}
}

func lastInvocation(t *testing.T, pctx *pipeline.Context) pipeline.Invocation {
	t.Helper()
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Outbound) == 0 {
		t.Fatal("no outbound invocation recorded")
	}
	inv := pctx.Extensions.Invocations.Outbound
	return inv[len(inv)-1]
}

// audOf decodes the aud claim of the token now on the request.
func audOf(t *testing.T, pctx *pipeline.Context) []string {
	t.Helper()
	tok := strings.TrimPrefix(pctx.Headers.Get("Authorization"), "Bearer ")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("Authorization is not a JWT: %q", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	// Keycloak encodes a single audience as a string, several as an array.
	var c struct {
		Aud json.RawMessage `json:"aud"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	var one string
	if json.Unmarshal(c.Aud, &one) == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(c.Aud, &many); err != nil {
		t.Fatalf("aud is neither a string nor an array: %s", c.Aud)
	}
	return many
}

func TestGrantedTargetIsExchangedAndCached(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	p := configured(t, srv, "")

	pctx := outbound("github-tool:9090", "Bearer user-token")
	if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("action = %v (%+v), want Continue", a.Type, a.Violation)
	}
	if got := audOf(t, pctx); len(got) != 1 || got[0] != "github-tool" {
		t.Errorf("forwarded token aud = %v, want [github-tool]", got)
	}
	if inv := lastInvocation(t, pctx); inv.Action != pipeline.ActionModify || inv.Reason != reasonExchanged {
		t.Errorf("invocation = %s/%s, want modify/%s", inv.Action, inv.Reason, reasonExchanged)
	}

	again := outbound("github-tool.team1.svc.cluster.local", "Bearer user-token")
	p.OnRequest(context.Background(), again)
	if inv := lastInvocation(t, again); inv.Reason != reasonCacheHit {
		t.Errorf("second request reason = %s, want %s", inv.Reason, reasonCacheHit)
	}
	if n := kc.calls.Load(); n != 1 {
		t.Errorf("IdP calls = %d, want 1 (second request served from cache)", n)
	}
}

// An exchange records a delegation hop, which the opa plugin turns into the
// outbound input.identity; a request that is not exchanged records none.
func TestExchangeRecordsDelegationHop(t *testing.T) {
	_, srv := newFakeKeycloak(t)
	p := configured(t, srv, "")

	for i, wantCache := range []bool{false, true} {
		pctx := outbound("github-tool", "Bearer user-token")
		p.OnRequest(context.Background(), pctx)
		d := pctx.Extensions.Delegation
		if d == nil || len(d.Chain()) != 1 {
			t.Fatalf("request %d: delegation = %+v, want one hop", i, d)
		}
		hop := d.Chain()[0]
		if hop.SubjectID != "alice" || hop.Audience != "github-tool" || hop.Strategy != PluginName || hop.FromCache != wantCache {
			t.Errorf("request %d: hop = %+v", i, hop)
		}
		if strings.Join(hop.Scopes, " ") != "openid github-read" {
			t.Errorf("request %d: scopes = %v", i, hop.Scopes)
		}
		if d.Origin != "alice" || d.Actor != "alice" {
			t.Errorf("request %d: origin/actor = %q/%q", i, d.Origin, d.Actor)
		}
	}

	for _, host := range []string{"ollama", "hr-tool", "api.openai.com"} {
		pctx := outbound(host, "Bearer user-token")
		p.OnRequest(context.Background(), pctx)
		if pctx.Extensions.Delegation != nil {
			t.Errorf("%s: delegation recorded for a request that was not exchanged", host)
		}
	}
}

// An agent is a target like any other: its audience is just another client.
func TestAgentTargetIsExchanged(t *testing.T) {
	_, srv := newFakeKeycloak(t)
	p := configured(t, srv, "")
	pctx := outbound("weather-agent", "Bearer user-token")
	p.OnRequest(context.Background(), pctx)
	if got := audOf(t, pctx); len(got) != 1 || got[0] != "weather-agent" {
		t.Errorf("aud = %v, want [weather-agent]", got)
	}
}

func TestUnknownAudiencePassesThroughAndIsRemembered(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	now := time.Unix(1_000_000, 0)
	p := New()
	p.now = func() time.Time { return now }
	cfg := fmt.Sprintf(`{"token_url":%q,"client_id":"agent","client_secret":"s3cret","unknown_audience_ttl":"1m"}`, srv.URL)
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatal(err)
	}

	pctx := outbound("ollama", "Bearer user-token")
	if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("action = %v (%+v), want Continue", a.Type, a.Violation)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer user-token" {
		t.Errorf("Authorization = %q, want it forwarded unchanged", got)
	}
	inv := lastInvocation(t, pctx)
	if inv.Action != pipeline.ActionSkip || inv.Reason != reasonAudienceUnknown || inv.Details["cached"] != "false" {
		t.Errorf("invocation = %s/%s %v", inv.Action, inv.Reason, inv.Details)
	}

	second := outbound("ollama", "Bearer user-token")
	p.OnRequest(context.Background(), second)
	if inv := lastInvocation(t, second); inv.Details["cached"] != "true" {
		t.Errorf("second request not answered from the unknown cache: %v", inv.Details)
	}
	if n := kc.calls.Load(); n != 1 {
		t.Errorf("IdP calls = %d, want 1", n)
	}

	now = now.Add(61 * time.Second)
	p.OnRequest(context.Background(), outbound("ollama", "Bearer user-token"))
	if n := kc.calls.Load(); n != 2 {
		t.Errorf("IdP calls after TTL = %d, want 2 (expired entry asks again)", n)
	}
}

func TestExistingButNotGrantedIsDenied(t *testing.T) {
	_, srv := newFakeKeycloak(t)
	p := configured(t, srv, "")
	pctx := outbound("hr-tool", "Bearer user-token")
	a := p.OnRequest(context.Background(), pctx)
	if a.Type != pipeline.Reject || a.Violation.Status != http.StatusForbidden {
		t.Fatalf("action = %v %+v, want 403 reject", a.Type, a.Violation)
	}
	if inv := lastInvocation(t, pctx); inv.Action != pipeline.ActionDeny || inv.Reason != reasonNotGranted {
		t.Errorf("invocation = %s/%s", inv.Action, inv.Reason)
	}
}

func TestIdPFailuresDenyNeverPassThrough(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, form map[string]string) bool{
		"server error": func(w http.ResponseWriter, _ map[string]string) bool {
			oauthError(w, 500, "", "")
			return true
		},
		"client disabled": func(w http.ResponseWriter, _ map[string]string) bool {
			oauthError(w, 400, "invalid_client", "Client disabled")
			return true
		},
		"non-JSON error": func(w http.ResponseWriter, _ map[string]string) bool {
			w.WriteHeader(400)
			_, _ = w.Write([]byte("<html>Audience not found</html>"))
			return true
		},
		// The IdP answers 200 with a token for some other audience.
		"wrong audience issued": func(w http.ResponseWriter, _ map[string]string) bool {
			issue(w, []string{"account", "weather-agent"})
			return true
		},
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			kc, srv := newFakeKeycloak(t)
			kc.override = override
			p := configured(t, srv, "")
			pctx := outbound("github-tool", "Bearer user-token")
			a := p.OnRequest(context.Background(), pctx)
			if a.Type != pipeline.Reject || a.Violation.Status != http.StatusServiceUnavailable {
				t.Fatalf("action = %v %+v, want 503 reject", a.Type, a.Violation)
			}
			if got := pctx.Headers.Get("Authorization"); got != "Bearer user-token" {
				t.Errorf("Authorization changed to %q on a denied request", got)
			}
		})
	}
}

func TestIdPUnreachableDenies(t *testing.T) {
	_, srv := newFakeKeycloak(t)
	p := configured(t, srv, "")
	srv.Close()
	a := p.OnRequest(context.Background(), outbound("github-tool", "Bearer user-token"))
	if a.Type != pipeline.Reject || a.Violation.Status != http.StatusServiceUnavailable {
		t.Fatalf("action = %v %+v, want 503 reject", a.Type, a.Violation)
	}
}

func TestHostsOutsideTheClusterNeverReachTheIdP(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	p := configured(t, srv, "")
	for _, host := range []string{"api.openai.com", "github-tool.evil.example", "10.1.2.3:443"} {
		pctx := outbound(host, "Bearer user-token")
		if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
			t.Errorf("%s: action = %v, want Continue", host, a.Type)
		}
		if got := pctx.Headers.Get("Authorization"); got != "Bearer user-token" {
			t.Errorf("%s: Authorization = %q, want unchanged", host, got)
		}
		if inv := lastInvocation(t, pctx); inv.Reason != reasonHostNotInCluster {
			t.Errorf("%s: reason = %s", host, inv.Reason)
		}
	}
	if n := kc.calls.Load(); n != 0 {
		t.Errorf("IdP calls = %d, want 0", n)
	}
}

func TestNoTokenClientCredentialsIsNarrowed(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	var mu sync.Mutex
	var grants []string
	kc.override = func(_ http.ResponseWriter, form map[string]string) bool {
		mu.Lock()
		defer mu.Unlock()
		grants = append(grants, form["grant_type"])
		return false
	}
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), grants...)
	}
	p := configured(t, srv, `,"no_token_policy":"client-credentials"`)

	pctx := outbound("github-tool", "")
	if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("action = %v %+v", a.Type, a.Violation)
	}
	// Keycloak's client_credentials token carries every granted audience; the
	// one forwarded must carry only the destination's.
	if got := audOf(t, pctx); len(got) != 1 || got[0] != "github-tool" {
		t.Errorf("forwarded aud = %v, want [github-tool]", got)
	}
	if g := seen(); len(g) != 2 || g[0] != "client_credentials" || g[1] != grantTokenExchange {
		t.Errorf("grants = %v, want client_credentials then token-exchange", g)
	}

	// The workload's own token is cached; a second target costs one exchange.
	p.OnRequest(context.Background(), outbound("weather-agent", ""))
	if g := seen(); len(g) != 3 {
		t.Errorf("grants after second target = %v, want one more exchange only", g)
	}

	// Discovery works without a user token too.
	unknown := outbound("ollama", "")
	p.OnRequest(context.Background(), unknown)
	if inv := lastInvocation(t, unknown); inv.Reason != reasonAudienceUnknown {
		t.Errorf("no-token unknown host reason = %s", inv.Reason)
	}
	if got := unknown.Headers.Get("Authorization"); got != "" {
		t.Errorf("unknown host got Authorization %q; the workload's own token must not leak", got)
	}
}

func TestNoTokenPolicies(t *testing.T) {
	kc, srv := newFakeKeycloak(t)

	allow := configured(t, srv, `,"no_token_policy":"allow"`)
	pctx := outbound("github-tool", "")
	if a := allow.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Errorf("allow: action = %v", a.Type)
	}
	if pctx.Headers.Get("Authorization") != "" {
		t.Error("allow added an Authorization header")
	}
	if inv := lastInvocation(t, pctx); inv.Reason != reasonNoToken {
		t.Errorf("allow: reason = %s, want %s", inv.Reason, reasonNoToken)
	}

	// deny is the default, as in token-exchange.
	for name, extra := range map[string]string{"explicit": `,"no_token_policy":"deny"`, "default": ""} {
		deny := configured(t, srv, extra)
		a := deny.OnRequest(context.Background(), outbound("github-tool", ""))
		if a.Type != pipeline.Reject || a.Violation.Status != http.StatusUnauthorized ||
			a.Violation.Code != "auth.unauthorized" || a.Violation.Reason != "missing Authorization header" {
			t.Errorf("deny (%s): action = %v %+v, want 401 auth.unauthorized", name, a.Type, a.Violation)
		}
	}
	if n := kc.calls.Load(); n != 0 {
		t.Errorf("IdP calls = %d, want 0", n)
	}
}

// subjectSeen records the subject_token of every token exchange.
func subjectSeen(kc *fakeKeycloak) func() []string {
	var mu sync.Mutex
	var subjects []string
	kc.override = func(_ http.ResponseWriter, form map[string]string) bool {
		if form["grant_type"] == grantTokenExchange {
			mu.Lock()
			subjects = append(subjects, form["subject_token"])
			mu.Unlock()
		}
		return false
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), subjects...)
	}
}

func withPlaceholder(t *testing.T, pctx *pipeline.Context, real string) string {
	t.Helper()
	st := memstore.New()
	handle, err := placeholder.New()
	if err != nil {
		t.Fatal(err)
	}
	st.Put(placeholder.Key(handle), real, time.Hour)
	pctx.Shared = st
	pctx.Headers.Set("Authorization", "Bearer "+handle)
	return handle
}

func TestPlaceholderIsResolvedBeforeExchange(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	subjects := subjectSeen(kc)
	p := configured(t, srv, `,"resolve_placeholders":true`)

	pctx := outbound("github-tool", "")
	withPlaceholder(t, pctx, "real-user-token")
	if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("action = %v %+v", a.Type, a.Violation)
	}
	if got := subjects(); len(got) != 1 || got[0] != "real-user-token" {
		t.Errorf("subject tokens = %v, want [real-user-token]", got)
	}
	if got := audOf(t, pctx); len(got) != 1 || got[0] != "github-tool" {
		t.Errorf("forwarded aud = %v, want [github-tool]", got)
	}
}

func TestPlaceholderIsNotResolvedByDefault(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	subjects := subjectSeen(kc)
	p := configured(t, srv, "")

	pctx := outbound("github-tool", "")
	handle := withPlaceholder(t, pctx, "real-user-token")
	p.OnRequest(context.Background(), pctx)
	if got := subjects(); len(got) != 1 || got[0] != handle {
		t.Errorf("subject tokens = %v, want the handle sent as is", got)
	}
}

func TestUnresolvablePlaceholderIsDeniedForEveryHost(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	p := configured(t, srv, `,"resolve_placeholders":true`)
	handle, _ := placeholder.New()

	for _, host := range []string{"github-tool", "api.openai.com"} {
		for name, shared := range map[string]bool{"no store": false, "unknown handle": true} {
			pctx := outbound(host, "Bearer "+handle)
			if shared {
				pctx.Shared = memstore.New()
			}
			a := p.OnRequest(context.Background(), pctx)
			if a.Type != pipeline.Reject || a.Violation.Code != "auth.unauthorized" {
				t.Errorf("%s, %s: action = %v %+v, want auth.unauthorized", host, name, a.Type, a.Violation)
				continue
			}
			if inv := lastInvocation(t, pctx); inv.Reason != reasonPlaceholderUnresolved {
				t.Errorf("%s, %s: reason = %s", host, name, inv.Reason)
			}
		}
	}
	if n := kc.calls.Load(); n != 0 {
		t.Errorf("IdP calls = %d, want 0", n)
	}
}

func TestPassedThroughRequestKeepsThePlaceholder(t *testing.T) {
	_, srv := newFakeKeycloak(t)
	p := configured(t, srv, `,"resolve_placeholders":true`)

	for _, host := range []string{"ollama", "api.openai.com"} {
		pctx := outbound(host, "")
		handle := withPlaceholder(t, pctx, "real-user-token")
		if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
			t.Fatalf("%s: action = %v %+v", host, a.Type, a.Violation)
		}
		if got := pctx.Headers.Get("Authorization"); got != "Bearer "+handle {
			t.Errorf("%s: Authorization = %q, want the placeholder, never the real token", host, got)
		}
	}
}

// The bearer is read with auth.ExtractBearer, as in token-exchange: the
// token is everything after "Bearer ", spaces included.
func TestBearerIsTakenVerbatim(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	subjects := subjectSeen(kc)
	p := configured(t, srv, "")

	p.OnRequest(context.Background(), outbound("github-tool", "bearer  user-token"))
	if got := subjects(); len(got) != 1 || got[0] != " user-token" {
		t.Errorf("subject tokens = %q, want [\" user-token\"]", got)
	}
}

func TestDeniesWhileCredentialsAreMissing(t *testing.T) {
	_, srv := newFakeKeycloak(t)
	p := New()
	dir := t.TempDir()
	cfg := fmt.Sprintf(`{"token_url":%q,"client_id_file":%q,"client_secret_file":%q}`,
		srv.URL, dir+"/id", dir+"/secret")
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatal(err)
	}
	if p.Ready() {
		t.Fatal("Ready() before credentials exist")
	}
	a := p.OnRequest(context.Background(), outbound("github-tool", "Bearer user-token"))
	if a.Type != pipeline.Reject || a.Violation.Status != http.StatusServiceUnavailable {
		t.Errorf("action = %v %+v, want 503", a.Type, a.Violation)
	}
	// A non-target is unaffected by missing credentials.
	if a := p.OnRequest(context.Background(), outbound("api.openai.com", "")); a.Type != pipeline.Continue {
		t.Errorf("non-target denied while not ready: %+v", a.Violation)
	}
}

func TestConfigure(t *testing.T) {
	cases := map[string]string{
		"no token url":   `{"client_id":"a","client_secret":"b"}`,
		"bad policy":     `{"token_url":"http://kc","no_token_policy":"exchange"}`,
		"passthrough":    `{"token_url":"http://kc","no_token_policy":"passthrough"}`,
		"bad template":   `{"token_url":"http://kc","audience_template":"fixed"}`,
		"bad ttl":        `{"token_url":"http://kc","unknown_audience_ttl":"-1s"}`,
		"bad timeout":    `{"token_url":"http://kc","timeout":"0s"}`,
		"unknown field":  `{"token_url":"http://kc","routes":{"file":"/etc/authproxy/routes.yaml"}}`,
		"token scopes":   `{"token_url":"http://kc","token_scopes":"openid"}`,
		"empty cluster":  `{"token_url":"http://kc","cluster_domain":"."}`,
		"malformed json": `{`,
	}
	for name, cfg := range cases {
		if err := New().Configure(json.RawMessage(cfg)); err == nil {
			t.Errorf("%s: Configure accepted %s", name, cfg)
		}
	}

	p := New()
	if err := p.Configure(json.RawMessage(`{"keycloak_url":"http://kc:8080/","keycloak_realm":"rossoctl","client_id":"a","client_secret":"b"}`)); err != nil {
		t.Fatal(err)
	}
	if want := "http://kc:8080/realms/rossoctl/protocol/openid-connect/token"; p.cfg.TokenURL != want {
		t.Errorf("derived token_url = %q, want %q", p.cfg.TokenURL, want)
	}
	if !p.Ready() {
		t.Error("inline credentials did not make the plugin ready")
	}
}

func TestUnconfiguredDenies(t *testing.T) {
	a := New().OnRequest(context.Background(), outbound("github-tool", "Bearer x"))
	if a.Type != pipeline.Reject {
		t.Errorf("unconfigured plugin let a request through")
	}
}

func TestInitPicksUpCredentialFilesThatArriveLater(t *testing.T) {
	_, srv := newFakeKeycloak(t)
	dir := t.TempDir()
	p := New()
	cfg := fmt.Sprintf(`{"token_url":%q,"client_id_file":%q,"client_secret_file":%q}`,
		srv.URL, dir+"/id", dir+"/secret")
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatal(err)
	}
	if err := p.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	if err := os.WriteFile(dir+"/id", []byte("agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/secret", []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !p.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("credentials written to disk were never loaded")
		}
		time.Sleep(50 * time.Millisecond)
	}
	pctx := outbound("github-tool", "Bearer user-token")
	if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("after load: %v %+v", a.Type, a.Violation)
	}
}
