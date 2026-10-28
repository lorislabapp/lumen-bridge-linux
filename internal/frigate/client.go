// Package frigate is a tiny HTTP client for Frigate's events API.
// Used by the bridge to fetch the event snapshot JPEG (at "new") and the
// finalised MP4 clip (at "end") so they can be uploaded as CKAssets on
// the FrigateEvent record.
//
// Point base_url at Frigate's internal port 5000. With Frigate 0.14+
// authentication enabled, port 8971 requires a login; port 5000 is the
// unauthenticated internal API and should only be reachable on your LAN.
package frigate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
}

// New builds a Frigate API client. baseURL is typically the same host
// as the MQTT broker on port 5000, e.g. http://frigate.local:5000.
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		// Clip fetches can take several seconds on slow disks; the
		// macOS bridge defaults to 60s. Match that.
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// FetchClip downloads the finalised MP4 for an event. Empty body + nil
// error is returned when the clip simply doesn't exist (Frigate kept the
// event metadata but skipped the recording).
func (c *Client) FetchClip(ctx context.Context, eventID string) ([]byte, error) {
	return c.fetch(ctx, eventID, "clip.mp4", 50*1024*1024)
}

// FetchSnapshot downloads the event's current best snapshot JPEG. Frigate
// serves it while the event is still in progress. (nil, nil) when Frigate
// has no snapshot for the event (snapshots disabled for that camera).
func (c *Client) FetchSnapshot(ctx context.Context, eventID string) ([]byte, error) {
	return c.fetch(ctx, eventID, "snapshot.jpg", 10*1024*1024)
}

func (c *Client) fetch(ctx context.Context, eventID, file string, limit int64) ([]byte, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("frigate base URL not configured")
	}
	u := fmt.Sprintf("%s/api/events/%s/%s", c.baseURL, url.PathEscape(eventID), file)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", file, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fetch %s: HTTP %d", file, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s body: %w", file, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("fetch %s: larger than %d bytes", file, limit)
	}
	return body, nil
}
