// Package frigate is a tiny HTTP client for Frigate's events API.
// Used by the bridge to fetch the finalised MP4 clip after an `end`
// event so it can be uploaded as a CKAsset on the FrigateEvent record.
//
// The mqtt package gives us snapshot bytes (Frigate publishes them
// retained on `frigate/{camera}/{label}/snapshot`); the clip needs to
// come from HTTP because Frigate doesn't republish the finalised MP4
// over MQTT — the file lives on disk and is served from
// `GET /api/events/{event_id}/clip.mp4`.
package frigate

import (
	"context"
	"fmt"
	"io"
	"net/http"
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
		baseURL: baseURL,
		// Clip fetches can take several seconds on slow disks / RTSP
		// streams; the macOS bridge defaults to 60s. Match that.
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// FetchClip downloads the finalised MP4 for an event. Returns the raw
// bytes ready to hand to cloudkit.UploadAsset. Empty body + nil error is
// returned when the clip simply doesn't exist (Frigate kept the event
// metadata but skipped the recording — common for low-confidence events).
func (c *Client) FetchClip(ctx context.Context, eventID string) ([]byte, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("frigate base URL not configured")
	}
	url := fmt.Sprintf("%s/api/events/%s/clip.mp4", c.baseURL, eventID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch clip: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Event exists but no clip on disk — return (nil, nil) so the
		// caller treats this as "no enrichment available" rather than
		// a hard error.
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fetch clip: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 50*1024*1024)) // 50 MB cap
	if err != nil {
		return nil, fmt.Errorf("read clip body: %w", err)
	}
	return body, nil
}
