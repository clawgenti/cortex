package tokenexchangeaiac

import "testing"

func TestResolveEligibleHosts(t *testing.T) {
	r, err := newTargetResolver("{service}", "cluster.local", "team1")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		host, service, namespace string
	}{
		{"github-tool", "github-tool", "team1"},
		{"github-tool:9090", "github-tool", "team1"},
		{"GitHub-Tool.", "github-tool", "team1"},
		{"github-tool.team2.svc", "github-tool", "team2"},
		{"github-tool.team2.svc.cluster.local", "github-tool", "team2"},
		{"github-tool.team2.svc.cluster.local:8080", "github-tool", "team2"},
	}
	for _, c := range cases {
		got, why := r.resolve(c.host)
		if why != "" {
			t.Errorf("resolve(%q): refused with %q, want eligible", c.host, why)
			continue
		}
		if got.service != c.service || got.namespace != c.namespace || got.audience != c.service {
			t.Errorf("resolve(%q) = %+v, want service=%s namespace=%s", c.host, got, c.service, c.namespace)
		}
	}
}

// The refusals are the security property: a host the workload can point at
// something outside the cluster must never be turned into an audience.
func TestResolveRefusesHostsOutsideTheCluster(t *testing.T) {
	r, err := newTargetResolver("{service}", "cluster.local", "team1")
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{
		"",
		"github-tool.evil.example",           // first label matches a client
		"github-tool.team1",                  // same shape as example.com
		"api.openai.com",                     //
		"github-tool.team1.svc.evil.example", // .svc in the middle
		"github-tool.team1.svc.cluster.local.evil", // suffix not at the end
		"a.github-tool.team1.svc.cluster.local",    // extra label
		"github-tool.team1.svc.other.domain",       // different cluster domain
		"10.0.0.7",
		"[::1]:8080",
		"bad_name",  // not an RFC 1123 label
		"-leading",  //
		"trailing-", //
	} {
		if got, why := r.resolve(host); why != reasonHostNotInCluster {
			t.Errorf("resolve(%q) = %+v, %q; want refusal %q", host, got, why, reasonHostNotInCluster)
		}
	}
}

func TestResolveSPIFFETemplate(t *testing.T) {
	r, err := newTargetResolver("spiffe://localtest.me/ns/{namespace}/sa/{service}", "cluster.local", "")
	if err != nil {
		t.Fatal(err)
	}
	got, why := r.resolve("weather-tool.team1.svc.cluster.local")
	if why != "" || got.audience != "spiffe://localtest.me/ns/team1/sa/weather-tool" {
		t.Errorf("FQDN: got %+v, %q", got, why)
	}
	// A short name carries no namespace, and none is configured.
	if _, why := r.resolve("weather-tool"); why != reasonNamespaceUnknown {
		t.Errorf("short name without namespace: reason %q, want %q", why, reasonNamespaceUnknown)
	}
}

func TestTemplateValidation(t *testing.T) {
	for _, tmpl := range []string{"{service}", "{service}-{namespace}", "spiffe://td/ns/{namespace}/sa/{service}"} {
		if err := validateTemplate(tmpl); err != nil {
			t.Errorf("validateTemplate(%q) = %v, want nil", tmpl, err)
		}
	}
	for _, tmpl := range []string{"", "static-audience", "{namespace}", "{service}-{svc}", "{service}}"} {
		if err := validateTemplate(tmpl); err == nil {
			t.Errorf("validateTemplate(%q) = nil, want an error", tmpl)
		}
	}
}
