package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/pricing"
)

// End to end through the real handler: a real Registry, the real JSON, the real
// renderer. A canned fixture would let the wire shape drift from the producer, which is
// the failure this whole area keeps having.
func realStatServer(t *testing.T) string {
	t.Helper()
	tab, err := pricing.Build(nil) // bundled rates + the shipped gateway discount
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(pricing.NewRegistry(tab).Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRunPricing_HostViewShowsTheDiscountApplied(t *testing.T) {
	var out, errb bytes.Buffer
	code := runPricing([]string{
		"--stats-url", realStatServer(t),
		"--host", "ete-litellm.ai-models.vpc-int.res.ibm.com",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	got := out.String()
	t.Logf("\n%s", got)

	// The measured gateway rates for opus-5: 3.8 in, 19 out.
	for _, want := range []string{"claude-opus-5", "3.8", "19", "multiplier 0.76", "ete-litellm"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q", want)
		}
	}
	// It must say the figures are scaled, or a reader compares them against a
	// published price list and concludes the tool is broken.
	if !strings.Contains(got, "vendor list scaled") {
		t.Errorf("output does not explain that rates are scaled: %s", got)
	}
	// The bundled table gives sonnet-4-5 a 200k long-context tier, and the host view
	// resolves at prompt size 0 — so its row MUST carry the marker and the footnote
	// must give the breakpoint. Quoting a below-threshold rate as if it were the only
	// rate is the exact failure this package exists to remove.
	//
	// BOTH breakpoints, because the table carries two: Anthropic prices above 200k and
	// OpenAI above 272k. This asserted the 200,000-only wording until the second one
	// was bundled, and the footnote is rendered by breakpointList, which sorts and
	// dedupes — so the one-breakpoint string was never the contract, just the only
	// output it could produce. Naming both keeps the assertion as strong as it was:
	// dropping either tier from the table still fails here.
	if !strings.Contains(got, "long-context rates apply above 200,000/272,000 prompt tokens") {
		t.Errorf("no long-context note naming both breakpoints: %s", got)
	}
	// The above-threshold rates must be present AND scaled: sonnet-4-5's 200k input
	// premium is 6.00 at list, so at 0.76 it is 4.56. Quoting 6.00 here would be the
	// unscaled figure; quoting nothing would leave the operator doing the arithmetic
	// this command exists to do.
	af := overrideAfter(t, got, "claude-sonnet-4-5")
	if len(af) != 7 {
		t.Fatalf("override line has %d columns, want 7: %q", len(af), af)
	}
	if af[3] != "4.56" {
		t.Errorf("above-threshold input = %q, want the scaled 4.56 (6.00 means unscaled)", af[3])
	}
	// A model without a tier gets no continuation line.
	if _, ok := overrideLineAfter(got, "claude-opus-5"); ok {
		t.Error("opus-5 has a long-context line, but it has no long-context tier")
	}
}

func TestRunPricing_UnscaledEndpointSaysSo(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runPricing([]string{"--stats-url", realStatServer(t), "--host", "api.anthropic.com"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "no gateway discount") {
		t.Errorf("expected the no-discount note: %s", got)
	}
	// UNSCALED vendor list for opus-5: 5 in, 25 out. Asserted by COLUMN, not by
	// Contains: "5" is a substring of the model name "claude-opus-5" and of "25", so a
	// containment check on either the output or the row proves nothing at all.
	//
	// Columns are: model, input, cache-write, cache-read, output, provenance.
	f := strings.Fields(modelRow(t, got, "claude-opus-5"))
	if len(f) < 6 {
		t.Fatalf("opus-5 row has %d columns, want 6: %q", len(f), f)
	}
	if f[1] != "5" {
		t.Errorf("input column = %q, want the list rate 5", f[1])
	}
	if f[4] != "25" {
		t.Errorf("output column = %q, want the list rate 25", f[4])
	}
	// No separate "and not 3.8/19" check: the two equalities above already exclude every
	// other value, discounted ones included.
}

// modelRow returns one model's line, so a rate assertion is scoped to that model rather
// than matching any digit anywhere in the table.
func modelRow(t *testing.T, out, model string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		// Exact first field, not Contains: "claude-opus-5" is a prefix of
		// "claude-opus-5-1", and a substring match would silently accept the wrong row.
		if f := strings.Fields(line); len(f) > 0 && f[0] == model {
			return line
		}
	}
	t.Fatalf("no %s row in:\n%s", model, out)
	return ""
}

func TestRunPricing_TableViewListsRowsAndDiscounts(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runPricing([]string{"--stats-url", realStatServer(t)}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{"Pricing table", "generated from litellm", "Gateway discounts", "res.ibm.com", "UNSCALED"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q", want)
		}
	}
}

// A local install prices from a downloaded list, and the header must say so.
func TestRunPricing_TableViewSaysWhenTheListWasDownloaded(t *testing.T) {
	all := [pricing.NumTiers]bool{}
	for i := range all {
		all[i] = true
	}
	tab, err := pricing.BuildWithList(nil, &pricing.List{
		Entries: []pricing.Entry{{Host: "*", Model: "claude-opus-5-5", Prov: pricing.ProvBundled,
			Rates: pricing.Rates{Base: [pricing.NumTiers]float64{pricing.TierInput: 4e-06, pricing.TierCacheWrite: 5e-06,
				pricing.TierCacheRead: 2e-07, pricing.TierOutput: 2e-05}, Set: all}}},
		FetchedAt: time.Date(2026, 10, 8, 21, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(pricing.NewRegistry(tab).Handler())
	t.Cleanup(srv.Close)

	var out, errb bytes.Buffer
	if code := runPricing([]string{"--stats-url", srv.URL}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "rates from litellm's price list, downloaded 2026-10-08 21:00 UTC") {
		t.Errorf("output does not say when the list was downloaded:\n%s", got)
	}
	if !strings.Contains(got, "bundled rates generated from litellm "+short(pricing.BundledUpstreamCommit)) {
		t.Errorf("output does not name the shipped commit, whose rows the list does not name are still in the table:\n%s", got)
	}
}

// A tier with no rate must render "-", never 0.00: pricing.Cost refuses to price a
// request that used such a tier, so a zero would misrepresent a coverage gap as free.
func TestRunPricing_UnsetTierRendersAsAbsent(t *testing.T) {
	if got := rate(0); got != "-" {
		t.Errorf("rate(0) = %q, want %q", got, "-")
	}
	if got := rate(3.8); got != "3.8" {
		t.Errorf("rate(3.8) = %q", got)
	}
}

func TestRunPricing_ProxyDownIsActionable(t *testing.T) {
	var out, errb bytes.Buffer
	code := runPricing([]string{"--stats-url", "http://127.0.0.1:1"}, &out, &errb)
	if code == 0 {
		t.Fatal("expected a non-zero exit when the proxy is unreachable")
	}
	if !strings.Contains(errb.String(), "agentop service status") {
		t.Errorf("error does not tell the operator what to check: %s", errb.String())
	}
}

// The raw view must not present a two-tier row as a one-tier answer. Four bundled models
// carry a 200k override; the wire has always had them and the renderer used to drop them.
func TestRunPricing_TableViewShowsLongContextOverrides(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runPricing([]string{"--stats-url", realStatServer(t)}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	// By column, not by Contains: a Contains on "        6 " would depend on the %9s
	// padding and would fail on a width change while the rate itself was correct.
	//
	// Override columns: "above", <breakpoint>, "tok:", then the four rates.
	f := overrideAfter(t, out.String(), "claude-sonnet-4-5")
	if len(f) != 7 {
		t.Fatalf("override line has %d columns, want 7: %q", len(f), f)
	}
	if f[1] != "200,000" {
		t.Errorf("breakpoint = %q, want 200,000", f[1])
	}
	// sonnet-4-5's 200k premium at list: 6.00 input, 22.50 output. The raw view is
	// unscaled, so these are list and not the discounted figures.
	if f[3] != "6" {
		t.Errorf("override input = %q, want the premium 6", f[3])
	}
	if f[6] != "22.5" {
		t.Errorf("override output = %q, want the premium 22.5", f[6])
	}
}

// overrideAfter returns the long-context continuation line that follows a model's row in
// the raw view, as columns.
//
// Positional on purpose: the continuation line only means anything attached to the row
// above it, so finding it by scanning for "above" anywhere would not prove it landed on
// the right model.
func overrideAfter(t *testing.T, out, model string) []string {
	t.Helper()
	f, ok := overrideLineAfter(out, model)
	if !ok {
		t.Fatalf("no long-context line after the %s row in:\n%s", model, tail(out))
	}
	return f
}

// overrideLineAfter returns the long-context continuation line following a model's row.
//
// Handles both views: the raw table leads with the endpoint (model is the second column),
// the host view leads with the model. Matching either position keeps one helper for both
// rather than two that can disagree about what "the line after" means.
func overrideLineAfter(out, model string) ([]string, bool) {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		f := strings.Fields(line)
		onRow := (len(f) > 0 && f[0] == model) || (len(f) > 1 && f[1] == model)
		if !onRow || i+1 >= len(lines) {
			continue
		}
		if nf := strings.Fields(lines[i+1]); len(nf) > 0 && nf[0] == "above" {
			return nf, true
		}
		return nil, false
	}
	return nil, false
}

func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	return strings.Join(lines, "\n")
}

// The per-Mtok sub-header does not claim dollars when the endpoint bills in something else.
//
// It was the literal "$/Mtok", printed four times — an ASSERTION rather than a rendering. Harmless
// while every endpoint billed in dollars; false the moment one declares `unit: credits`, and a
// rate quoted in the wrong currency is the silent-wrong-number failure this package exists to
// remove, arriving through the tool built to inspect it.
func TestRunPricing_DoesNotClaimDollarsForANonUSDEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"host":"api.us-east.bob.ibm.com","models":[
			{"model":"premium-ide","provenance":"configured","unit":"credits",
			 "inputPerMillion":2,"cacheWritePerMillion":2,"cacheReadPerMillion":2,
			 "outputPerMillion":2}]}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runPricing([]string{"--stats-url", srv.URL, "--host", "api.us-east.bob.ibm.com"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	if strings.Contains(got, "$/Mtok") {
		t.Errorf("a credits endpoint's rates are labelled $/Mtok:\n%s", got)
	}
	// And the unit it IS in has to appear, or the figures are unlabelled rather than mislabelled —
	// which is not an improvement.
	if !strings.Contains(got, "credits") {
		t.Errorf("output never names the unit the rates are in:\n%s", got)
	}
}

// A USD endpoint still says $/Mtok, character for character.
//
// The fix must not cost every existing reader the label they already had: this is every deployment
// today, and replacing "$/Mtok" with a bare "/Mtok" everywhere would make the common case less
// informative to avoid a lie in the rare one.
func TestRunPricing_StillSaysDollarsForAUSDEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"host":"api.anthropic.com","models":[
			{"model":"claude-opus-5","provenance":"bundled",
			 "inputPerMillion":5,"cacheWritePerMillion":6.25,"cacheReadPerMillion":0.5,
			 "outputPerMillion":25}]}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runPricing([]string{"--stats-url", srv.URL, "--host", "api.anthropic.com"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	if got := out.String(); !strings.Contains(got, "$/Mtok") {
		t.Errorf("a USD endpoint lost its $/Mtok label:\n%s", got)
	}
}

// A server that spells the default "usd" still gets the dollar label, on both readers.
//
// THE CLIENT SIDE OF THE CASE-FOLDING FIX. core canonicalises before it serialises, so a matched
// pair never sends this — but agentop is a client of whatever proxy it is pointed at, including one
// older than itself, and both readers here compared against pricing.CurrencyUSD case-sensitively.
// The symptom is not a wrong figure: it is "per Mtok" over a table of dollars and a provenance cell
// reading "bundled · usd", which is the same false claim as the one this change removes, inverted.
//
// BOTH READERS IN ONE TEST, because they are one question asked twice — the sub-header and the
// per-row cell — and they disagreeing is the defect isDefaultUnit exists to make impossible.
func TestRunPricing_ALowercaseUSDFromTheServerIsStillTheDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"host":"gw.example","models":[
			{"model":"some-model","provenance":"configured","unit":"usd",
			 "inputPerMillion":2,"cacheWritePerMillion":2,"cacheReadPerMillion":2,
			 "outputPerMillion":2}]}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runPricing([]string{"--stats-url", srv.URL, "--host", "gw.example"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()

	if !strings.Contains(got, "$/Mtok") {
		t.Errorf(`a "usd" endpoint lost the $/Mtok label; dollars are being called a foreign unit:\n%s`, got)
	}
	if strings.Contains(got, "per Mtok") {
		t.Errorf(`a "usd" endpoint rendered the non-USD sub-header:\n%s`, got)
	}
	// And the provenance cell does not append a unit for the default, whatever its spelling.
	if strings.Contains(got, "· usd") {
		t.Errorf("the provenance cell names the default as though it were a foreign unit:\n%s", got)
	}
}

// The two unit predicates agree, directly, for every spelling that reaches them.
//
// Asserted on the functions rather than only through rendered output: the rendering tests above can
// only reach the pair through one endpoint shape each, and the property is about the predicate.
func TestIsDefaultUnit_EveryDefaultSpellingAndNoOthers(t *testing.T) {
	for _, unit := range []string{"", "USD", "usd", "Usd", "uSd"} {
		if !isDefaultUnit(unit) {
			t.Errorf("isDefaultUnit(%q) = false, want true", unit)
		}
		if got := provenanceCell("configured", unit); got != "configured" {
			t.Errorf("provenanceCell(%q) = %q, want the bare provenance", unit, got)
		}
	}
	for _, unit := range []string{"credits", "Bobcoins", "USDC", "usdt"} {
		if isDefaultUnit(unit) {
			t.Errorf("isDefaultUnit(%q) = true; a real unit was swallowed as the default", unit)
		}
		if got := provenanceCell("configured", unit); !strings.Contains(got, unit) {
			t.Errorf("provenanceCell(%q) = %q, which drops the unit", unit, got)
		}
	}
}

// OpenCode Zen's free models are priced at zero, and their rate fields arrive omitted like
// an unset tier's. Rendered as "-" they would read as a coverage gap, the opposite of what
// they are. And they are Zen's: another endpoint's view must not list them at all.
func TestRunPricing_ZenFreeModelsRenderAsFree(t *testing.T) {
	url := realStatServer(t)

	var out, errb bytes.Buffer
	if code := runPricing([]string{"--stats-url", url, "--host", "opencode.ai"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	// Columns are: model, input, cache-write, cache-read, output, provenance.
	for _, m := range []string{"big-pickle", "*-free"} {
		f := strings.Fields(modelRow(t, out.String(), m))
		if len(f) < 6 || f[1] != "free" || f[2] != "free" || f[3] != "free" || f[4] != "free" {
			t.Errorf("%s row = %q, want every rate column \"free\"", m, f)
		}
	}

	out.Reset()
	if code := runPricing([]string{"--stats-url", url, "--host", "api.anthropic.com"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(line); len(f) > 0 && (f[0] == "big-pickle" || f[0] == "*-free") {
			t.Errorf("api.anthropic.com lists Zen's free model: %q", line)
		}
	}

	out.Reset()
	if code := runPricing([]string{"--stats-url", url}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	// Table view columns are: endpoint, model, input, cache-write, cache-read, output, from.
	var saw bool
	for _, line := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(line); len(f) >= 7 && f[0] == "opencode.ai" && f[1] == "big-pickle" {
			saw = true
			if f[2] != "free" || f[5] != "free" {
				t.Errorf("table row = %q, want its rates \"free\"", f)
			}
		}
	}
	if !saw {
		t.Errorf("no opencode.ai big-pickle row in the table view:\n%s", out.String())
	}
}
