package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/money"
)

// runPricing renders the rates the running proxy is actually using.
//
// This exists because no config file can answer the question. `pricing:` shows what the
// operator wrote; the figures a request is charged come from that PLUS a rate table
// compiled into the binary, and on a local install downloaded from LiteLLM, PLUS any
// shipped gateway discount. Storing the table in the
// config instead would freeze every install at the rates current on its install date,
// silently, because that file is written once and never refreshed — which is the exact
// staleness the pricing work was done to remove.
//
// So: not stored, inspectable. And with --host it applies host scoping, specificity,
// provenance precedence and the multiplier together, which is not something a reader
// can carry out by eye over a rate table.
func runPricing(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agentop pricing", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", "", "show the rates this endpoint is actually charged, discount applied")
	asJSON := fs.Bool("json", false, "emit the raw JSON from the proxy")
	statsURL := fs.String("stats-url", defaultCortexStatsURL, "base URL of the proxy's stat server")
	fs.Usage = func() {
		fmt.Fprint(stderr, `agentop pricing — show the model rates Cortex is using

Usage:
  agentop pricing                     every row in the table, unscaled
  agentop pricing --host <gateway>    what that endpoint is charged, discount applied
  agentop pricing --json              raw JSON

The rates come from a table built into the binary (vendor list), your `+"`pricing:`"+`
config, and any gateway discount Cortex ships. A local install updates the built-in table
from LiteLLM's price list, downloaded hourly, so a new model is priced without a new
release. --host is the useful form: it resolves all of it the way a request would.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	u := strings.TrimSuffix(*statsURL, "/") + "/pricing/table"
	if *host != "" {
		// Escaped, not concatenated: a host is operator-supplied and a "&" or "#" in
		// it would silently truncate the parameter, so the proxy would answer for the
		// wrong endpoint — a wrong answer presented as a right one.
		u += "?" + url.Values{"host": {*host}}.Encode()
	}
	body, err := fetchPricing(u)
	if err != nil {
		fmt.Fprintf(stderr, "agentop pricing: %v\n", err)
		fmt.Fprintf(stderr, "  is Cortex running? `agentop service status`\n")
		return 1
	}
	if *asJSON {
		fmt.Fprintln(stdout, string(body))
		return 0
	}
	if *host != "" {
		return renderEffective(body, stdout, stderr)
	}
	return renderTable(body, stdout, stderr)
}

func fetchPricing(url string) ([]byte, error) {
	c := &http.Client{Timeout: 10 * time.Second}
	// Request carries a context even though the client bounds the deadline: it is what
	// lets a caller cancel, and it keeps this call the same shape as the rest of agentop.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot build a request for %s: %w", url, err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the stat server at %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s returned 404 — this proxy predates the pricing endpoint", url)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// effective mirrors pricing.Effective. Declared here rather than imported so agentop
// keeps decoding a wire shape rather than linking the pricing package's internals; the
// field names are what the JSON contract pins.
type effective struct {
	Host           string  `json:"host"`
	Multiplier     float64 `json:"multiplier"`
	MultiplierFrom string  `json:"multiplierFrom"`
	Models         []struct {
		Model      string  `json:"model"`
		Provenance string  `json:"provenance"`
		Unpriced   bool    `json:"unpriced"`
		Free       bool    `json:"free"`
		Unit       string  `json:"unit,omitempty"`
		LongCtx    int     `json:"longContextAbove"`
		AboveIn    float64 `json:"aboveInputPerMillion"`
		AboveCW    float64 `json:"aboveCacheWritePerMillion"`
		AboveCR    float64 `json:"aboveCacheReadPerMillion"`
		AboveOut   float64 `json:"aboveOutputPerMillion"`
		In         float64 `json:"inputPerMillion"`
		CW         float64 `json:"cacheWritePerMillion"`
		CR         float64 `json:"cacheReadPerMillion"`
		Out        float64 `json:"outputPerMillion"`
	} `json:"models"`
}

func renderEffective(body []byte, stdout, stderr io.Writer) int {
	var e effective
	if err := json.Unmarshal(body, &e); err != nil {
		fmt.Fprintf(stderr, "agentop pricing: unreadable response: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Rates in effect for %s\n\n", e.Host)
	fmt.Fprintf(stdout, "  %-30s %9s %9s %9s %9s  %s\n", "model", "input", "cache-wr", "cache-rd", "output", "provenance")
	// PER Mtok IN WHOSE CURRENCY, taken from the rows rather than asserted. This line read
	// "$/Mtok" as a literal, four times, which was a rendering while every endpoint billed in
	// dollars and became a false claim the moment one could declare `unit: credits`.
	anyNonUSD := false
	for _, m := range e.Models {
		if !isDefaultUnit(m.Unit) {
			anyNonUSD = true
			break
		}
	}
	perMtok := perMtokLabel(anyNonUSD)
	fmt.Fprintf(stdout, "  %-30s %9s %9s %9s %9s\n", "", perMtok, perMtok, perMtok, perMtok)
	var breakpoints []int
	for _, m := range e.Models {
		if m.Unpriced {
			fmt.Fprintf(stdout, "  %-30s %9s %9s %9s %9s  %s\n", m.Model, "-", "-", "-", "-", "UNPRICED")
			continue
		}
		c := rateCells(m.Free, m.In, m.CW, m.CR, m.Out)
		fmt.Fprintf(stdout, "  %-30s %9s %9s %9s %9s  %s\n", m.Model,
			c[0], c[1], c[2], c[3], provenanceCell(m.Provenance, m.Unit))
		// The above-threshold rates, discount already applied, on a continuation line.
		// Naming the breakpoint alone still left the operator to work out what their long
		// sessions cost — by reading the raw table and applying the factor by hand, which
		// is the arithmetic this command exists to do.
		if m.LongCtx > 0 {
			breakpoints = append(breakpoints, m.LongCtx)
			fmt.Fprintf(stdout, "    %-28s %9s %9s %9s %9s\n",
				"above "+commas(m.LongCtx)+" tok:",
				rate(m.AboveIn), rate(m.AboveCW), rate(m.AboveCR), rate(m.AboveOut))
		}
	}
	// The footer used to call the rates "vendor list" unconditionally, directly beneath
	// rows that may read `configured` — an operator's own pinned figures described back
	// to them as the vendor's. Keyed off what the rows actually say instead.
	base := "vendor list"
	if allConfigured(e) {
		base = "your configured rates"
	} else if anyConfigured(e) {
		base = "vendor list except where a row reads `configured`"
	}
	if e.Multiplier != 1 {
		fmt.Fprintf(stdout, "\n  multiplier %.4g applied, from the %s rules — rates above are %s scaled by it\n",
			e.Multiplier, e.MultiplierFrom, base)
	} else {
		fmt.Fprintf(stdout, "\n  no gateway discount applies to this endpoint; rates above are %s\n", base)
	}
	if len(breakpoints) > 0 {
		// "its breakpoint", not "that breakpoint": the list can hold more than one now
		// (Anthropic prices above 200k, OpenAI above 272k), and the singular read as
		// though every model shared the one figure.
		fmt.Fprintf(stdout, "  long-context rates apply above %s prompt tokens; the indented line under a\n"+
			"  model is what a request past its own breakpoint is charged\n", breakpointList(breakpoints))
	}
	return 0
}

// rate formats a per-million figure, or "-" for a tier with no rate. A tier with no
// rate is not free: pricing.Cost refuses to price a request that used one, so showing
// 0.00 would misrepresent a coverage gap as a price.
func rate(v float64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%.4g", v)
}

// rateCells formats a row's four per-million rates. A row the proxy marks free prices every
// tier at zero, and its rate fields arrive omitted like an unset tier's, so without the
// flag a free model would render as "-" — a coverage gap — which is the opposite of what
// it is.
func rateCells(free bool, in, cw, cr, out float64) [4]string {
	if free {
		return [4]string{"free", "free", "free", "free"}
	}
	return [4]string{rate(in), rate(cw), rate(cr), rate(out)}
}

// thresholdRow is one long-context override on a raw row.
//
// Rendered rather than dropped: the raw view already warns that its rates are unscaled,
// and staying silent about a second tier would leave the same gap in the other direction
// — a figure presented as the whole answer when a longer request is charged more.
type thresholdRow struct {
	Above int     `json:"abovePromptTokens"`
	In    float64 `json:"inputPerMillion"`
	CW    float64 `json:"cacheWritePerMillion"`
	CR    float64 `json:"cacheReadPerMillion"`
	Out   float64 `json:"outputPerMillion"`
}

type describeBody struct {
	UpstreamCommit string    `json:"upstreamCommit"`
	ListFetchedAt  time.Time `json:"listFetchedAt"`
	Rows           []struct {
		Host       string         `json:"host"`
		Model      string         `json:"model"`
		Provenance string         `json:"provenance"`
		Free       bool           `json:"free"`
		In         float64        `json:"inputPerMillion"`
		CW         float64        `json:"cacheWritePerMillion"`
		CR         float64        `json:"cacheReadPerMillion"`
		Out        float64        `json:"outputPerMillion"`
		Thresholds []thresholdRow `json:"thresholds"`
	} `json:"rows"`
	Multipliers []struct {
		Host       string  `json:"host"`
		Factor     float64 `json:"factor"`
		Provenance string  `json:"provenance"`
	} `json:"multipliers"`
}

func renderTable(body []byte, stdout, stderr io.Writer) int {
	var d describeBody
	if err := json.Unmarshal(body, &d); err != nil {
		fmt.Fprintf(stderr, "agentop pricing: unreadable response: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Pricing table (%d rows)\n", len(d.Rows))
	if d.UpstreamCommit != "" {
		fmt.Fprintf(stdout, "  bundled rates generated from litellm %s\n", short(d.UpstreamCommit))
	}
	if !d.ListFetchedAt.IsZero() {
		fmt.Fprintf(stdout, "  rates from litellm's price list, downloaded %s\n", d.ListFetchedAt.UTC().Format("2006-01-02 15:04 MST"))
	}
	fmt.Fprintf(stdout, "\n  %-22s %-30s %9s %9s %9s %9s  %s\n",
		"endpoint", "model", "input", "cache-wr", "cache-rd", "output", "from")
	for _, r := range d.Rows {
		h := r.Host
		if h == "" || h == "*" {
			h = "(any)"
		}
		c := rateCells(r.Free, r.In, r.CW, r.CR, r.Out)
		fmt.Fprintf(stdout, "  %-22s %-30s %9s %9s %9s %9s  %s\n", h, r.Model,
			c[0], c[1], c[2], c[3], r.Provenance)
		// Overrides on a continuation line, so the row above is not silently the
		// below-threshold half of a two-tier answer. A tier the override leaves unset
		// renders "-", meaning "inherits the row above", which is what At() does.
		for _, th := range r.Thresholds {
			fmt.Fprintf(stdout, "  %-22s   %-28s %9s %9s %9s %9s\n", "",
				"above "+commas(th.Above)+" tok:",
				rate(th.In), rate(th.CW), rate(th.CR), rate(th.Out))
		}
	}
	if len(d.Multipliers) > 0 {
		fmt.Fprintf(stdout, "\nGateway discounts\n\n  %-30s %8s  %s\n", "endpoint", "factor", "from")
		for _, m := range d.Multipliers {
			h := m.Host
			if h == "" || h == "*" {
				h = "(any)"
			}
			fmt.Fprintf(stdout, "  %-30s %8.4g  %s\n", h, m.Factor, m.Provenance)
		}
		fmt.Fprintf(stdout, "\n  Rates above are UNSCALED. Use --host <endpoint> to see what one is charged.\n")
	}
	return 0
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// breakpointList renders the distinct long-context breakpoints, largest last.
//
// Distinct, because four models sharing one 200k threshold should read "200,000", not
// the same number four times.
func breakpointList(ns []int) string {
	sort.Ints(ns)
	var out []string
	for i, n := range ns {
		if i > 0 && n == ns[i-1] {
			continue
		}
		out = append(out, commas(n))
	}
	return strings.Join(out, "/")
}

// commas groups a token count for reading: 200000 is hard to size at a glance, 200,000
// is not, and these numbers only ever appear in prose meant for a human.
func commas(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// allConfigured / anyConfigured report how much of a host view is the operator's own
// pinning, so the footer can describe the rates it is actually printing.
func allConfigured(e effective) bool {
	n := 0
	for _, m := range e.Models {
		if m.Unpriced {
			continue
		}
		if m.Provenance != "configured" {
			return false
		}
		n++
	}
	return n > 0
}

func anyConfigured(e effective) bool {
	for _, m := range e.Models {
		if !m.Unpriced && m.Provenance == "configured" {
			return true
		}
	}
	return false
}

// perMtokLabel is the per-million-tokens sub-header for the units present in a table.
//
// "$/Mtok" WHEN EVERY ROW IS USD, character for character as before — which is every deployment
// today. Replacing it with a bare "/Mtok" everywhere would make the common case less informative
// in order to avoid a lie in the rare one.
//
// "per Mtok" AS SOON AS ONE ROW IS NOT, because no single currency in this header can be right for
// every row once they differ, and naming one of them would mislabel the others. The unit then
// rides on each ROW instead, beside its provenance, where it is per-figure and cannot be wrong.
//
// A SUB-HEADER RATHER THAN A NEW COLUMN, deliberately: the unit is absent from every row in the
// overwhelmingly common case, and a permanently empty column costs every reader width to say
// nothing.
func perMtokLabel(anyNonUSD bool) string {
	if anyNonUSD {
		return "per Mtok"
	}
	return "$/Mtok"
}

// isDefaultUnit reports whether a unit off the wire means the default, USD.
//
// ONE PREDICATE FOR BOTH READERS — the "$/Mtok" sub-header and the provenance cell — because they
// are two renderings of one question, and an exact comparison makes them disagree: a server
// sending "usd" would have both of them call dollars a foreign unit.
//
// FOLDED ON THIS SIDE TOO, though core now canonicalises before it serialises. agentop is a client of
// whatever server it is pointed at, including one older than itself, and a rate label is exactly
// the kind of cosmetic disagreement nobody would think to look for after a partial upgrade.
func isDefaultUnit(unit string) bool { return money.IsDefault(unit) }

// provenanceCell is a row's provenance, with its unit appended when that unit is not the default.
//
// The unit sits here because provenance is already the row's "where did this come from" cell, and a
// currency is part of that answer. Empty means USD — the wire omits the default — so the common row
// is unchanged.
func provenanceCell(prov, unit string) string {
	if isDefaultUnit(unit) {
		return prov
	}
	return prov + " · " + unit
}
