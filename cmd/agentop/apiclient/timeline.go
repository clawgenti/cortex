package apiclient

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/rossoctl/cortex/core/session/timeline"
)

// GetTimeline fetches the session's spans, clipped to [from, to] when either is non-zero.
// ErrNotFound means an unknown session, or a proxy too old to serve the endpoint; the two
// share a 404, and the Timeline pane says which is likelier.
func (c *Client) GetTimeline(ctx context.Context, id string, from, to time.Time) (*timeline.Timeline, error) {
	q := url.Values{}
	if !from.IsZero() {
		q.Set("from", from.UTC().Format(time.RFC3339Nano))
	}
	if !to.IsZero() {
		q.Set("to", to.UTC().Format(time.RFC3339Nano))
	}
	path := fmt.Sprintf("/v1/sessions/%s/timeline", url.PathEscape(id))
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var tl timeline.Timeline
	if err := c.getJSON(ctx, path, &tl); err != nil {
		return nil, err
	}
	return &tl, nil
}
