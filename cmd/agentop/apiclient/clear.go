package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ClearResult is DELETE /v1/sessions' answer: what went from memory, and what went from disk.
type ClearResult struct {
	Sessions         int   `json:"sessions"`
	ArchivedSessions int   `json:"archivedSessions"`
	Bytes            int64 `json:"bytes"`
	// ArchiveError is why the disk half failed. Memory is cleared even then.
	ArchiveError string `json:"archiveError,omitempty"`
}

// ErrClearUnsupported is a proxy that predates DELETE /v1/sessions.
var ErrClearUnsupported = errors.New("this Cortex predates clearing sessions; upgrade it")

// ErrClearRefused is a 403: the proxy does not accept a clear from here. The error carries its
// reason.
var ErrClearRefused = errors.New("clear refused")

// clearTimeout bounds ClearSessions when the caller set no deadline. Longer than
// restDefaultTimeout because the server waits up to 10s for its archive before answering; a
// client giving up at the same 10s would report a clear that happened as one that failed.
var clearTimeout = 30 * time.Second

// ErrSessionGone is a 404 from DELETE /v1/sessions/{id}: this Cortex holds no such session, in
// memory or on disk — erased already, or never there.
var ErrSessionGone = errors.New("this Cortex holds no such session")

// ErrDeleteUnsupported is a proxy that predates DELETE /v1/sessions/{id}. It answers 405, because
// GET is registered on the same path.
var ErrDeleteUnsupported = errors.New("this Cortex predates erasing single sessions; upgrade it")

// ClearSessions asks the proxy to clear every session, in memory and on disk. A 500 still
// returns the counts — memory was cleared — with ArchiveError saying why the disk half was not.
func (c *Client) ClearSessions(ctx context.Context) (ClearResult, error) {
	const path = "/v1/sessions"
	code, body, err := c.sendDelete(ctx, path)
	if err != nil {
		return ClearResult{}, err
	}
	if code == http.StatusMethodNotAllowed || code == http.StatusNotFound {
		return ClearResult{}, ErrClearUnsupported
	}
	return eraseResult(path, code, body)
}

// DeleteSession asks the proxy to erase one session, in memory and on disk. Answers as
// ClearSessions, plus ErrSessionGone for a 404 and ErrDeleteUnsupported for a 405.
func (c *Client) DeleteSession(ctx context.Context, id string) (ClearResult, error) {
	path := "/v1/sessions/" + pathSegment(id)
	code, body, err := c.sendDelete(ctx, path)
	if err != nil {
		return ClearResult{}, err
	}
	switch code {
	case http.StatusNotFound:
		return ClearResult{}, ErrSessionGone
	case http.StatusMethodNotAllowed:
		return ClearResult{}, ErrDeleteUnsupported
	}
	return eraseResult(path, code, body)
}

// pathSegment escapes id as exactly one path segment. url.PathEscape leaves "." and ".." alone,
// since dots are unreserved, and a mux cleans those segments away: DELETE /v1/sessions/. would be
// redirected to /v1/sessions, the route that clears every session.
func pathSegment(id string) string {
	switch id {
	case ".":
		return "%2E"
	case "..":
		return "%2E%2E"
	}
	return url.PathEscape(id)
}

// sendDelete sends DELETE path and returns the status and the body, bounded.
//
// ITS OWN REQUEST, not getBody's: that one is GET-only, and it keeps the body of a 400 alone,
// where this has to read the body of a 403 and of a 500 too.
func (c *Client) sendDelete(ctx context.Context, path string) (int, []byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, clearTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint+path, nil)
	if err != nil {
		return 0, nil, err
	}
	// Never follow a redirect: an erase must reach only the route it named, and a 307 re-sends the
	// DELETE wherever Location points. A per-call copy, so the shared client keeps its policy.
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	// Bounded like every other body this client reads from a peer it cannot verify. 4KB rather
	// than badRequestDetail's 512, because archiveError carries the archive's paths, and a body
	// cut short of its closing brace would lose the counts with it.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, body, nil
}

// eraseResult maps the answers both erase routes share: the counts on a 200 and on a 500 —
// memory was erased, ArchiveError says why disk was not — and the server's reason on a 403.
func eraseResult(path string, code int, body []byte) (ClearResult, error) {
	switch code {
	case http.StatusOK, http.StatusInternalServerError:
		var res ClearResult
		if err := json.Unmarshal(body, &res); err != nil {
			return ClearResult{}, fmt.Errorf("%s: unexpected status %d", path, code)
		}
		res.ArchiveError = serverText(res.ArchiveError)
		return res, nil
	case http.StatusForbidden:
		if detail := badRequestDetail(body); detail != "" {
			return ClearResult{}, fmt.Errorf("%w: %s", ErrClearRefused, detail)
		}
		return ClearResult{}, ErrClearRefused
	}
	return ClearResult{}, fmt.Errorf("%s: unexpected status %d", path, code)
}
