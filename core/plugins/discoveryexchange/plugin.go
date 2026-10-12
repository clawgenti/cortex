// Package discoveryexchange is an outbound token-exchange plugin that needs no
// routes. It derives a target audience from the destination host and lets the
// IdP decide what that destination is: a token comes back for a target this
// caller is granted, "no such client" means the destination is not a target and
// the request passes through, and every other answer denies.
//
// It shares no code with token-exchange, by design: the two are alternatives,
// and a fix to one must not silently change the other. See
// docs/discovery-exchange-plugin.md.
package discoveryexchange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
)

// PluginName is the name the plugin registers under and is configured by.
const PluginName = "discovery-exchange"

// no_token_policy values.
const (
	noTokenClientCredentials = "client-credentials"
	noTokenPassthrough       = "passthrough"
	noTokenDeny              = "deny"
)

// Invocation reasons, machine-stable.
const (
	reasonExchanged         = "token_exchanged"
	reasonCacheHit          = "cache_hit"
	reasonAudienceUnknown   = "audience_unknown"
	reasonNoToken           = "no_token"
	reasonNotGranted        = "audience_not_granted"
	reasonExchangeFailed    = "exchange_failed"
	reasonCredentialsAbsent = "credentials_not_ready"
)

// selfTokenKey caches the caller's own client-credentials token. It cannot
// collide with a tokenKey, which always starts with 64 hex digits.
const selfTokenKey = "\x00self"

type discoveryExchangeConfig struct {
	// TokenURL is the OAuth token endpoint. When empty it is derived from
	// KeycloakURL and KeycloakRealm.
	TokenURL      string `json:"token_url" description:"OAuth token endpoint. Required unless keycloak_url and keycloak_realm are set."`
	KeycloakURL   string `json:"keycloak_url" description:"Keycloak base URL (internal service URL), used to derive token_url."`
	KeycloakRealm string `json:"keycloak_realm" description:"Keycloak realm, used to derive token_url."`

	ClientID         string `json:"client_id" description:"This workload's OAuth client ID. Overrides client_id_file."`
	ClientIDFile     string `json:"client_id_file" description:"File holding the client ID." default:"/shared/client-id.txt"`
	ClientSecret     string `json:"client_secret" description:"This workload's OAuth client secret. Overrides client_secret_file."`
	ClientSecretFile string `json:"client_secret_file" description:"File holding the client secret." default:"/shared/client-secret.txt"`

	// AudienceTemplate maps a destination to the audience requested for it.
	AudienceTemplate string `json:"audience_template" description:"Audience requested for a destination. Placeholders: {service}, {namespace}." default:"{service}"`
	ClusterDomain    string `json:"cluster_domain" description:"Cluster DNS domain; hosts ending in .svc.<cluster_domain> are in-cluster." default:"cluster.local"`
	Namespace        string `json:"namespace" description:"Namespace assumed for short service names, for templates that use {namespace}. Typically ${POD_NAMESPACE}."`

	NoTokenPolicy      string `json:"no_token_policy" description:"Outbound request with no bearer: exchange the workload's own client-credentials token, pass through, or deny." default:"client-credentials" enum:"client-credentials,passthrough,deny"`
	UnknownAudienceTTL string `json:"unknown_audience_ttl" description:"How long an audience the IdP does not know is passed through without asking again. 0 asks every time." default:"5m"`
	Timeout            string `json:"timeout" description:"Timeout for each call to the token endpoint." default:"5s"`
}

func (c *discoveryExchangeConfig) applyDefaults() {
	if c.TokenURL == "" && c.KeycloakURL != "" && c.KeycloakRealm != "" {
		c.TokenURL = strings.TrimSuffix(c.KeycloakURL, "/") + "/realms/" + c.KeycloakRealm + "/protocol/openid-connect/token"
	}
	if c.ClientID == "" && c.ClientIDFile == "" {
		c.ClientIDFile = "/shared/client-id.txt"
	}
	if c.ClientSecret == "" && c.ClientSecretFile == "" {
		c.ClientSecretFile = "/shared/client-secret.txt"
	}
	if c.AudienceTemplate == "" {
		c.AudienceTemplate = placeholderService
	}
	if c.ClusterDomain == "" {
		c.ClusterDomain = "cluster.local"
	}
	if c.NoTokenPolicy == "" {
		c.NoTokenPolicy = noTokenClientCredentials
	}
	if c.UnknownAudienceTTL == "" {
		c.UnknownAudienceTTL = "5m"
	}
	if c.Timeout == "" {
		c.Timeout = "5s"
	}
}

// settings are the parsed forms of the config's string-typed fields.
type settings struct {
	unknownTTL time.Duration
	timeout    time.Duration
}

func (c *discoveryExchangeConfig) validate() (settings, error) {
	var s settings
	if c.TokenURL == "" {
		return s, errors.New("token_url is required (or keycloak_url and keycloak_realm)")
	}
	switch c.NoTokenPolicy {
	case noTokenClientCredentials, noTokenPassthrough, noTokenDeny:
	default:
		return s, fmt.Errorf("no_token_policy must be %s, %s or %s, got %q",
			noTokenClientCredentials, noTokenPassthrough, noTokenDeny, c.NoTokenPolicy)
	}
	var err error
	if s.unknownTTL, err = time.ParseDuration(c.UnknownAudienceTTL); err != nil || s.unknownTTL < 0 {
		return s, fmt.Errorf("unknown_audience_ttl %q is not a non-negative duration", c.UnknownAudienceTTL)
	}
	if s.timeout, err = time.ParseDuration(c.Timeout); err != nil || s.timeout <= 0 {
		return s, fmt.Errorf("timeout %q is not a positive duration", c.Timeout)
	}
	return s, nil
}

// DiscoveryExchange is the plugin. The zero value denies every request until
// Configure succeeds.
type DiscoveryExchange struct {
	cfg      discoveryExchangeConfig
	idp      *idpClient
	targets  *targetResolver
	tokens   *tokenCache
	unknown  *unknownCache
	creds    atomic.Pointer[credentials]
	bgCancel atomic.Pointer[context.CancelFunc]

	// now is the clock for both caches; tests replace it.
	now func() time.Time

	exchanged, cacheHits, unknownPassed, notEligible, denied atomic.Uint64
}

// New constructs an unconfigured plugin.
func New() *DiscoveryExchange { return &DiscoveryExchange{now: time.Now} }

func init() {
	plugins.RegisterPlugin(PluginName, func() pipeline.Plugin { return New() })
}

func (p *DiscoveryExchange) Name() string { return PluginName }

func (p *DiscoveryExchange) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		Description: "Exchanges outbound tokens for an audience derived from the destination host; the IdP decides which hosts are targets. No routes.",
	}
}

func (p *DiscoveryExchange) ConfigSchema() []pipeline.FieldSchema {
	return pipeline.SchemaOf(discoveryExchangeConfig{})
}

func (p *DiscoveryExchange) Configure(raw json.RawMessage) error {
	var c discoveryExchangeConfig
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("%s config: %w", PluginName, err)
		}
	}
	c.applyDefaults()
	s, err := c.validate()
	if err != nil {
		return fmt.Errorf("%s config: %w", PluginName, err)
	}
	targets, err := newTargetResolver(c.AudienceTemplate, c.ClusterDomain, c.Namespace)
	if err != nil {
		return fmt.Errorf("%s config: %w", PluginName, err)
	}
	if targets.usesNamespace() && c.Namespace == "" {
		slog.Warn(PluginName+": audience_template uses {namespace} and namespace is unset; "+
			"short service names will pass through without an exchange",
			"audience_template", c.AudienceTemplate)
	}

	// Best-effort synchronous credential load; Init polls for whatever is
	// missing. Logged so a wrong path shows up in the first pod's logs.
	if c.ClientID == "" {
		if v, err := config.ReadCredentialFile(c.ClientIDFile); err == nil {
			c.ClientID = v
		} else {
			slog.Warn(PluginName+": client_id_file not yet readable; Init will poll",
				"path", c.ClientIDFile, "error", err)
		}
	}
	if c.ClientSecret == "" {
		if v, err := config.ReadCredentialFile(c.ClientSecretFile); err == nil {
			c.ClientSecret = v
		} else {
			slog.Warn(PluginName+": client_secret_file not yet readable; Init will poll",
				"path", c.ClientSecretFile, "error", err)
		}
	}

	// Commit only after everything fallible has succeeded.
	if p.now == nil {
		p.now = time.Now
	}
	p.cfg = c
	p.idp = &idpClient{tokenURL: c.TokenURL, http: &http.Client{Timeout: s.timeout}}
	p.targets = targets
	p.tokens = newTokenCache(p.now)
	p.unknown = newUnknownCache(p.now, s.unknownTTL)
	if c.ClientID != "" && c.ClientSecret != "" {
		p.creds.Store(&credentials{clientID: c.ClientID, clientSecret: c.ClientSecret})
	}
	return nil
}

// Init starts a background wait for credential files Configure could not read.
// The wait runs on its own context, cancelled by Shutdown, because Init's ctx
// only bounds pipeline start-up.
func (p *DiscoveryExchange) Init(_ context.Context) error {
	if p.creds.Load() != nil || p.bgCancel.Load() != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.bgCancel.Store(&cancel)
	go p.pollCredentials(ctx)
	return nil
}

func (p *DiscoveryExchange) pollCredentials(ctx context.Context) {
	id, secret := p.cfg.ClientID, p.cfg.ClientSecret
	var err error
	if id == "" {
		if id, err = config.WaitForCredentialFile(ctx, p.cfg.ClientIDFile); err != nil {
			return
		}
	}
	if secret == "" {
		if secret, err = config.WaitForCredentialFile(ctx, p.cfg.ClientSecretFile); err != nil {
			return
		}
	}
	p.creds.Store(&credentials{clientID: id, clientSecret: secret})
	slog.Info(PluginName + ": credentials loaded")
}

func (p *DiscoveryExchange) Shutdown(_ context.Context) error {
	if cancel := p.bgCancel.Swap(nil); cancel != nil {
		(*cancel)()
	}
	return nil
}

// Ready holds the pod out of service until client credentials are loaded.
func (p *DiscoveryExchange) Ready() bool { return p.creds.Load() != nil }

func (p *DiscoveryExchange) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.idp == nil {
		return pipeline.DenyStatus(http.StatusServiceUnavailable, "upstream.unreachable", PluginName+" not configured")
	}

	t, why := p.targets.resolve(pctx.Host)
	if why != "" {
		p.notEligible.Add(1)
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: why,
			Details: map[string]string{"host": pctx.Host}})
		return pipeline.Action{Type: pipeline.Continue}
	}
	details := map[string]string{"host": pctx.Host, "target_audience": t.audience}

	if p.unknown.has(t.audience) {
		return p.passUnknown(pctx, details, true)
	}

	creds := p.creds.Load()
	if creds == nil {
		return p.deny(pctx, details, reasonCredentialsAbsent, http.StatusServiceUnavailable,
			"upstream.token-exchange-failed", "client credentials are not loaded yet")
	}

	subject := bearer(pctx.Headers.Get("Authorization"))
	if subject == "" {
		switch p.cfg.NoTokenPolicy {
		case noTokenPassthrough:
			pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: reasonNoToken, Details: details})
			return pipeline.Action{Type: pipeline.Continue}
		case noTokenDeny:
			return p.deny(pctx, details, reasonNoToken, http.StatusUnauthorized,
				"auth.unauthorized", "outbound request carries no bearer token")
		}
		self, err := p.selfToken(ctx, *creds)
		if err != nil {
			slog.Warn(PluginName+": client-credentials grant failed", "error", err)
			return p.deny(pctx, details, reasonExchangeFailed, http.StatusServiceUnavailable,
				"upstream.token-exchange-failed", "could not obtain this workload's own token")
		}
		subject = self
	}

	key := tokenKey(subject, t.audience)
	if tok, ok := p.tokens.get(key); ok {
		p.cacheHits.Add(1)
		return p.replace(pctx, details, tok, true)
	}

	issued, err := p.idp.exchange(ctx, *creds, subject, t.audience)
	if err == nil {
		p.tokens.put(key, issued)
		p.exchanged.Add(1)
		return p.replace(pctx, details, issued.accessToken, false)
	}
	switch classify(err) {
	case outcomeUnknownAudience:
		p.unknown.add(t.audience)
		return p.passUnknown(pctx, details, false)
	case outcomeNotGranted:
		slog.Info(PluginName+": audience exists but is not granted to this workload",
			"host", pctx.Host, "audience", t.audience)
		return p.deny(pctx, details, reasonNotGranted, http.StatusForbidden,
			"policy.forbidden", "this workload is not granted the destination's audience")
	default:
		// Logged, not returned: the error can name other audiences the
		// caller holds, and the response body goes back to the workload.
		slog.Warn(PluginName+": token exchange failed", "host", pctx.Host, "audience", t.audience, "error", err)
		return p.deny(pctx, details, reasonExchangeFailed, http.StatusServiceUnavailable,
			"upstream.token-exchange-failed", "token exchange failed")
	}
}

// selfToken returns this workload's own client-credentials token, cached.
func (p *DiscoveryExchange) selfToken(ctx context.Context, creds credentials) (string, error) {
	if tok, ok := p.tokens.get(selfTokenKey); ok {
		return tok, nil
	}
	issued, err := p.idp.clientCredentials(ctx, creds)
	if err != nil {
		return "", err
	}
	p.tokens.put(selfTokenKey, issued)
	return issued.accessToken, nil
}

func (p *DiscoveryExchange) replace(pctx *pipeline.Context, details map[string]string, token string, cacheHit bool) pipeline.Action {
	pctx.Headers.Set("Authorization", "Bearer "+token)
	reason := reasonExchanged
	if cacheHit {
		reason = reasonCacheHit
	}
	details["cache_hit"] = boolString(cacheHit)
	pctx.Record(pipeline.Invocation{Action: pipeline.ActionModify, Reason: reason, Details: details})
	return pipeline.Action{Type: pipeline.Continue}
}

// passUnknown forwards a request to a destination the IdP has no client for,
// unchanged — including whatever Authorization header the workload sent.
func (p *DiscoveryExchange) passUnknown(pctx *pipeline.Context, details map[string]string, cached bool) pipeline.Action {
	p.unknownPassed.Add(1)
	details["cached"] = boolString(cached)
	pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: reasonAudienceUnknown, Details: details})
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *DiscoveryExchange) deny(pctx *pipeline.Context, details map[string]string, reason string, status int, code, msg string) pipeline.Action {
	p.denied.Add(1)
	pctx.Record(pipeline.Invocation{Action: pipeline.ActionDeny, Reason: reason, Details: details})
	return pipeline.DenyStatus(status, code, msg)
}

func (p *DiscoveryExchange) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// Metrics reports counters since the pipeline was built.
func (p *DiscoveryExchange) Metrics() []pipeline.Metric {
	return []pipeline.Metric{
		{Name: PluginName + ".exchanged", Value: float64(p.exchanged.Load()), Unit: "count", Note: "tokens issued by the IdP"},
		{Name: PluginName + ".cache_hits", Value: float64(p.cacheHits.Load()), Unit: "count"},
		{Name: PluginName + ".unknown_passthrough", Value: float64(p.unknownPassed.Load()), Unit: "count", Note: "requests to destinations the IdP has no client for"},
		{Name: PluginName + ".not_eligible", Value: float64(p.notEligible.Load()), Unit: "count", Note: "requests to hosts not offered to the IdP"},
		{Name: PluginName + ".denied", Value: float64(p.denied.Load()), Unit: "count"},
	}
}

// bearer returns the token from an "Authorization: Bearer <token>" value, or "".
func bearer(h string) string {
	const prefix = "bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

var (
	_ pipeline.Configurable    = (*DiscoveryExchange)(nil)
	_ pipeline.Initializer     = (*DiscoveryExchange)(nil)
	_ pipeline.Shutdowner      = (*DiscoveryExchange)(nil)
	_ pipeline.Readier         = (*DiscoveryExchange)(nil)
	_ pipeline.SchemaProvider  = (*DiscoveryExchange)(nil)
	_ pipeline.MetricsProvider = (*DiscoveryExchange)(nil)
)
