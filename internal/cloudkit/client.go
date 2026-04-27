// Package cloudkit speaks Apple's CloudKit Web Services REST API.
//
// Two auth modes are supported:
//
//   - "apiToken" (Web Services): a public-ish container token plus a
//     per-user iCloud session token (`X-CloudKit-UserAuth`) obtained via
//     the iCloud sign-in web flow. This is the recommended mode for
//     end-user-installed Linux Bridges — each user authenticates once and
//     the daemon writes only to their own private database.
//
//   - "serverToServer": ECDSA P-256 keypair where the public key is
//     registered with the CloudKit container. The private key signs every
//     request. This mode bypasses per-user auth and writes via the server's
//     identity — appropriate only for headless backends managed by the
//     container owner. Not used by Linux Bridge.
//
// API reference: https://developer.apple.com/library/archive/documentation/DataManagement/Conceptual/CloudKitWebServicesReference/
package cloudkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	// Apple's public CloudKit Web Services endpoint. The path embeds the
	// container ID and environment; we build the full URL per request.
	baseURL = "https://api.apple-cloudkit.com/database/1"
)

type Environment string

const (
	EnvProduction  Environment = "production"
	EnvDevelopment Environment = "development"
)

// Database scope. CloudKit serves three databases per container; we only
// write to the user's private one (each user's events are theirs alone).
type Database string

const (
	DBPrivate Database = "private"
	DBPublic  Database = "public"
	DBShared  Database = "shared"
)

type Client struct {
	container   string
	environment Environment
	apiToken    string // container API token (public)
	userToken   string // per-user iCloud session token

	httpClient *http.Client
	logger     *slog.Logger
}

type Options struct {
	Container   string
	Environment Environment
	APIToken    string
	UserToken   string
	Logger      *slog.Logger
}

func New(opts Options) *Client {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{
		container:   opts.Container,
		environment: opts.Environment,
		apiToken:    opts.APIToken,
		userToken:   opts.UserToken,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		logger:      opts.Logger.With("component", "cloudkit"),
	}
}

// SaveRecord upserts a single CKRecord-shaped JSON record into the user's
// private database. CloudKit Web Services accepts a batch of operations
// per request; we send a single-element batch for simplicity. Snapshot /
// clip CKAsset uploads are out-of-band and will be added in v0.3.0.
func (c *Client) SaveRecord(ctx context.Context, db Database, rec *Record) error {
	body := map[string]any{
		"operations": []any{
			map[string]any{
				"operationType": "forceUpdate", // upsert: don't fail on conflict
				"record":        rec.toJSON(),
			},
		},
	}
	url := fmt.Sprintf("%s/%s/%s/%s/records/modify",
		baseURL, c.container, c.environment, db)

	resp, err := c.do(ctx, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("modify records: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("modify records: HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, url string, body any) (*http.Response, error) {
	var buf io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		buf = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, buf)
	if err != nil {
		return nil, err
	}

	q := req.URL.Query()
	q.Set("ckAPIToken", c.apiToken)
	if c.userToken != "" {
		q.Set("ckSession", c.userToken)
	}
	req.URL.RawQuery = q.Encode()

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "LumenBridgeLinux/0.0.1")

	c.logger.Debug("CloudKit request", "method", method, "path", req.URL.Path)
	return c.httpClient.Do(req)
}
