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
//
// The Worker uses Event.ID as both a tracing tag (frigateID field on the
// CloudKit record) AND as the recordName base (`frigate-<id>`) so that
// Phase 2.5 clip-backfill can locate the same record by name without
// keeping any state on the bridge side.
type Event struct {
	ID         string   `json:"id"`         // Frigate event id — used as recordName base
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

// AssetReceipt is the shape CloudKit returns after a successful PUT
// to a presigned asset URL. Each field is required to commit the asset
// to the FrigateEvent record (Worker side does the commit).
type AssetReceipt struct {
	FileChecksum      string `json:"fileChecksum"`
	Size              int64  `json:"size"`
	Receipt           string `json:"receipt"`
	WrappingKey       string `json:"wrappingKey,omitempty"`
	ReferenceChecksum string `json:"referenceChecksum,omitempty"`
}

// AssetField names the bridge is allowed to upload. Mirrors the
// allowlist on the Worker side — kept here as constants so callers
// can't typo the string.
const (
	AssetFieldSnapshot = "snapshot"
	AssetFieldClip     = "clip"
)

// RequestUploadResponse mirrors the Worker's 200 body for
// /assets/request-upload. The url is a presigned Apple CDN endpoint;
// recordName is what the Worker (or CloudKit) decided to call the
// future record, so the bridge passes it back later when committing.
type RequestUploadResponse struct {
	URL        string `json:"url"`
	RecordName string `json:"recordName"`
	FieldName  string `json:"fieldName"`
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
// If snapshot is non-nil, the Bridge has already uploaded the JPEG
// bytes via UploadAsset and is committing the receipt to the same
// record being created here. When snapshot is nil the record is created
// metadata-only — that's the Phase 2 baseline behavior.
//
// Errors include the HTTP status code so the coordinator can decide
// whether to retry (5xx → transient, backoff) vs surface the failure
// (4xx → permanent, log and skip).
func (c *Client) PostEvent(ctx context.Context, e Event, snapshot *AssetReceipt) (*PostEventResponse, error) {
	body, err := json.Marshal(struct {
		Event    Event         `json:"event"`
		Snapshot *AssetReceipt `json:"snapshot,omitempty"`
	}{Event: e, Snapshot: snapshot})
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

// RequestAssetUpload asks the Worker for a presigned CloudKit upload
// URL. fieldName must be one of AssetFieldSnapshot / AssetFieldClip
// (Worker rejects anything else). frigateID, if non-empty, becomes
// the recordName base so a later UpdateEventAsset can target the
// same record.
//
// Round trip is ~150 ms (sign + Apple call). Caller should hold the
// returned URL only briefly — Apple's presigned URLs are valid for
// 10 minutes per the S2S signature window.
func (c *Client) RequestAssetUpload(ctx context.Context, fieldName, frigateID string) (*RequestUploadResponse, error) {
	if fieldName != AssetFieldSnapshot && fieldName != AssetFieldClip {
		return nil, fmt.Errorf("invalid asset fieldName %q (must be %q or %q)",
			fieldName, AssetFieldSnapshot, AssetFieldClip)
	}
	body, err := json.Marshal(map[string]string{
		"fieldName": fieldName,
		"frigateID": frigateID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal asset request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/assets/request-upload", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build asset request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request upload: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("relay /assets/request-upload %d: %s",
			resp.StatusCode, truncate(string(respBody), 200))
	}
	var out RequestUploadResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode upload response: %w (raw=%s)", err,
			truncate(string(respBody), 200))
	}
	if out.URL == "" {
		return nil, fmt.Errorf("worker returned empty upload url")
	}
	return &out, nil
}

// UploadAsset PUTs raw bytes to a presigned CloudKit asset URL and
// returns the AssetReceipt CloudKit returned. The receipt is what we
// pass back to the Worker via PostEvent (snapshot) or UpdateEventAsset
// (clip) to commit the asset onto a record.
//
// CloudKit's asset upload endpoint expects multipart/form-data with a
// single file field. We construct the multipart body inline to avoid
// pulling in mime/multipart for what is, mechanically, a one-liner.
func (c *Client) UploadAsset(ctx context.Context, uploadURL string, payload []byte, contentType string) (*AssetReceipt, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("empty asset payload")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Multipart envelope. CloudKit's upload endpoint accepts the file
	// under the form field "files". We pick a static, ASCII-safe
	// boundary — there's no user-controlled bytes that could collide.
	const boundary = "lumen-bridge-asset-boundary-v1"
	header := fmt.Sprintf(
		"--%s\r\nContent-Disposition: form-data; name=\"files\"; filename=\"asset\"\r\nContent-Type: %s\r\n\r\n",
		boundary, contentType,
	)
	trailer := fmt.Sprintf("\r\n--%s--\r\n", boundary)
	body := make([]byte, 0, len(header)+len(payload)+len(trailer))
	body = append(body, []byte(header)...)
	body = append(body, payload...)
	body = append(body, []byte(trailer)...)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upload request: %w", err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))

	// Asset uploads can be slow (large MP4 + uplink). Use a per-call
	// client with a generous timeout instead of the default 15s.
	uploadClient := &http.Client{Timeout: 120 * time.Second}
	resp, err := uploadClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("put asset bytes: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cloudkit asset upload %d: %s",
			resp.StatusCode, truncate(string(respBody), 200))
	}
	var ck struct {
		SingleFile *AssetReceipt `json:"singleFile"`
	}
	if err := json.Unmarshal(respBody, &ck); err != nil {
		return nil, fmt.Errorf("decode upload response: %w (raw=%s)", err,
			truncate(string(respBody), 200))
	}
	if ck.SingleFile == nil || ck.SingleFile.Receipt == "" {
		return nil, fmt.Errorf("cloudkit returned no singleFile receipt (raw=%s)",
			truncate(string(respBody), 200))
	}
	return ck.SingleFile, nil
}

// UpdateEventAsset commits an already-uploaded AssetReceipt onto an
// existing FrigateEvent record (typically the clip field at event-end).
// recordName must match what RequestAssetUpload returned — the Worker
// uses Frigate's event id as the recordName base, so this is just
// `frigate-<id>` in practice.
func (c *Client) UpdateEventAsset(ctx context.Context, recordName, fieldName string, asset *AssetReceipt) error {
	if recordName == "" {
		return fmt.Errorf("recordName required")
	}
	if fieldName != AssetFieldSnapshot && fieldName != AssetFieldClip {
		return fmt.Errorf("invalid asset fieldName %q", fieldName)
	}
	if asset == nil || asset.Receipt == "" {
		return fmt.Errorf("asset receipt required")
	}
	body, err := json.Marshal(struct {
		RecordName string        `json:"recordName"`
		FieldName  string        `json:"fieldName"`
		Asset      *AssetReceipt `json:"asset"`
	}{recordName, fieldName, asset})
	if err != nil {
		return fmt.Errorf("marshal update: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/events/update-asset", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build update: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("post update: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("relay /events/update-asset %d: %s",
			resp.StatusCode, truncate(string(respBody), 200))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
