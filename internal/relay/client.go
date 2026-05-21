// Package relay implements the HTTP client to the Lumen Bridge Relay
// Worker. The Worker holds the only CloudKit Server-to-Server private
// key and signs records on the Bridge's behalf, indexed by the device
// token issued during pairing.
//
// This replaces the older `internal/cloudkit` direct-CloudKit path, which
// depended on a ckSession token harvested via browser sign-in — Apple's
// IDMSA web auth is unreliable for multi-user shipping (see
// memory/project_relay_proxy_migration.md in the Lumen project notes).
//
// All operations are stateless from the Bridge's perspective: the bearer
// token in the `Authorization` header is the entire credential. Token
// rotation lives in the relay; the bridge never refreshes credentials
// on its own.
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Event is the relay-side projection of an mqtt.Event. We keep it as a
// dedicated struct (not a re-export) so the wire format is decoupled from
// the MQTT-side decoding — adding a field there shouldn't accidentally
// leak into the Worker contract.
type Event struct {
	ID         string   `json:"id"`         // Frigate event id — surfaced for tracing only
	Camera     string   `json:"camera"`
	Label      string   `json:"label"`
	DetectedAt int64    `json:"detectedAt"` // unix ms — Worker stores as DATE/TIME
	TopScore   float64  `json:"topScore"`
	Zones      []string `json:"zones"`
}

// PostEventResponse mirrors the Worker's 200 body.
type PostEventResponse struct {
	RecordName string `json:"recordName"`
	UserRef    string `json:"userRef"`
}

// Options configures the client. RelayURL must be a fully-qualified base
// (e.g. https://relay.lorislab.fr). DeviceToken is the bearer credential
// from a successful pair flow.
type Options struct {
	RelayURL    string
	DeviceToken string
	HTTPClient  *http.Client // optional — defaults to a 15s-timeout client
	Logger      *slog.Logger
}

type Client struct {
	baseURL string
	token   string
	http    *http.Client
	logger  *slog.Logger
}

// New builds a relay client. Returns an error when the URL is malformed
// or the token is empty — both are fatal misconfigurations the caller
// should fail loudly on rather than retry.
func New(opts Options) (*Client, error) {
	if opts.RelayURL == "" {
		return nil, errors.New("relay url is required")
	}
	if opts.DeviceToken == "" {
		return nil, errors.New("device token is required (run `lumen-bridge pair` first)")
	}
	if _, err := url.Parse(opts.RelayURL); err != nil {
		return nil, fmt.Errorf("invalid relay url: %w", err)
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		baseURL: strings.TrimRight(opts.RelayURL, "/"),
		token:   opts.DeviceToken,
		http:    httpClient,
		logger:  logger.With("component", "relay-client"),
	}, nil
}

// PostEvent forwards one Frigate event to the Worker. Returns the
// recordName CloudKit assigned (useful for clip backfill correlation in
// a follow-up commit) and the userRef the Worker tagged the record with.
//
// Errors include the HTTP status code so the coordinator can decide
// whether to retry (5xx → transient, backoff) vs surface the failure
// (4xx → permanent, log and skip).
func (c *Client) PostEvent(ctx context.Context, e Event) (*PostEventResponse, error) {
	body, err := json.Marshal(struct {
		Event Event `json:"event"`
	}{Event: e})
	if err != nil {
		return nil, fmt.Errorf("marshal event: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/events/ingest", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post event: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("relay /events/ingest %d: %s",
			resp.StatusCode, truncate(string(respBody), 200))
	}

	var parsed PostEventResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w (raw=%s)", err,
			truncate(string(respBody), 200))
	}
	return &parsed, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
