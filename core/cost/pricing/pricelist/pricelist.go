// Package pricelist keeps vendor list prices current by downloading LiteLLM's public price
// map while the proxy runs, rather than only when the binary is built.
//
// The shipped table (pricing.Bundled) is generated from the same map, so a downloaded list is
// the shipped table brought up to date: same transform, same rows, newer data. That is the
// whole fix for a model released after the build — LiteLLM listed claude-opus-5-5 on
// 2026-09-22, a week before its first request here, and the build in use priced it as
// claude-opus-5 for nine days.
package pricelist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/pricing/internal/pricegen"
)

// DefaultURL is LiteLLM's price map, the file bundled.go is generated from.
const DefaultURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// maxBytes bounds one download. The map is 3.1 MB today; this leaves room for it to grow
// without letting a misbehaving server fill memory.
const maxBytes = 16 << 20

// Fetcher downloads the price map and keeps the last good copy on disk.
//
// Not safe for concurrent use. The proxy has one caller, Run.
type Fetcher struct {
	// URL is where the price map is read from; DefaultURL when empty.
	URL string
	// CacheFile holds the last list that downloaded and parsed, so a restart prices from it
	// before the network answers, or without the network at all. Empty keeps nothing on disk.
	CacheFile string
	// Client makes the request; one with a 30s timeout when nil.
	Client *http.Client
	// Now stamps FetchedAt; time.Now when nil.
	Now func() time.Time

	// etag is the version of the list last applied. Sent as If-None-Match, so an unchanged
	// list costs a 304 with no body — which is what makes checking every hour free.
	etag string
}

// cacheFile is CacheFile's contents. Prices is the map already reduced to the rows the
// table reads (pricegen.Filter): kilobytes, where the download is megabytes.
type cacheFile struct {
	ETag      string          `json:"etag,omitempty"`
	FetchedAt time.Time       `json:"fetchedAt"`
	Prices    json.RawMessage `json:"prices"`
}

// Cached returns the list saved by an earlier download, or nil when there is none.
//
// A file that cannot be read back is an error rather than nil, so the caller can say the
// saved copy was ignored; the next download replaces it either way.
func (f *Fetcher) Cached() (*pricing.List, error) {
	if f.CacheFile == "" {
		return nil, nil
	}
	b, err := os.ReadFile(f.CacheFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c cacheFile
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("pricelist: %s: %w", f.CacheFile, err)
	}
	entries, err := pricegen.Entries(c.Prices)
	if err != nil {
		return nil, fmt.Errorf("pricelist: %s: %w", f.CacheFile, err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("pricelist: %s has no %s row with a rate", f.CacheFile, pricegen.BundledProviders())
	}
	f.etag = c.ETag
	return &pricing.List{Entries: entries, FetchedAt: c.FetchedAt}, nil
}

// Fetch downloads the list if it changed since the last one applied. It returns nil and no
// error when it has not.
//
// A list that cannot be used — an error status, a body that is not the price map, one with
// no rows the table reads — is an error and leaves the saved copy alone, so a bad upstream
// day never replaces a working table.
func (f *Fetcher) Fetch(ctx context.Context) (*pricing.List, error) {
	url := f.URL
	if url == "" {
		url = DefaultURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if f.etag != "" {
		req.Header.Set("If-None-Match", f.etag)
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, nil
	case http.StatusOK:
	default:
		return nil, fmt.Errorf("pricelist: %s answered %s", url, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBytes {
		return nil, fmt.Errorf("pricelist: %s is larger than %d bytes", url, maxBytes)
	}
	prices, err := pricegen.Filter(raw, "")
	if err != nil {
		return nil, err
	}
	entries, err := pricegen.Entries(prices)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("pricelist: %s has no %s row with a rate", url, pricegen.BundledProviders())
	}
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	l := &pricing.List{Entries: entries, FetchedAt: now()}
	etag := resp.Header.Get("ETag")
	if err := f.save(cacheFile{ETag: etag, FetchedAt: l.FetchedAt, Prices: prices}); err != nil {
		// The list is good and is applied; only the copy a restart would start from is
		// stale. The ETag is not kept, so the next check downloads and tries to save again.
		slog.Warn("pricelist: could not save the price list; a restart will start from the previous copy",
			"file", f.CacheFile, "error", err)
		return l, nil
	}
	f.etag = etag
	return l, nil
}

// save writes c to CacheFile through a rename, so a crash mid-write leaves the previous copy.
func (f *Fetcher) save(c cacheFile) error {
	if f.CacheFile == "" {
		return nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.CacheFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".price-list-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.CacheFile)
}

// Run applies the saved list, then checks for a newer one now and every interval until ctx
// ends, applying each change.
//
// A failed check keeps whatever is applied and is logged once per run of failures: a laptop
// offline for an afternoon should not log a warning an hour.
func (f *Fetcher) Run(ctx context.Context, every time.Duration, apply func(*pricing.List)) {
	if l, err := f.Cached(); err != nil {
		slog.Warn("pricelist: ignoring the saved price list", "file", f.CacheFile, "error", err)
	} else if l != nil {
		apply(l)
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	failing := false
	for {
		l, err := f.Fetch(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return
		case err != nil:
			if !failing {
				slog.Warn("pricelist: could not download the price list; prices stay as they are until it can",
					"error", err)
			}
			failing = true
		default:
			if failing {
				slog.Info("pricelist: the price list can be downloaded again")
			}
			failing = false
			if l != nil {
				apply(l)
				slog.Info("pricelist: prices updated from the downloaded list", "models", len(l.Entries))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
