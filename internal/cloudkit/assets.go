package cloudkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// UploadAsset performs the two-step CloudKit Web Services asset upload:
//
//   1. POST .../assets/upload with the target (record type, record name,
//      field name) → CloudKit returns a one-shot upload URL.
//   2. POST the bytes to that URL → CloudKit returns a "receipt" that
//      we attach to the record's field on the next records/modify call.
//
// Returns the AssetReceipt to embed in the record. The caller is then
// responsible for issuing a SaveRecord with the asset field populated as
// {"value": <receipt>, "type": "ASSETID"}.
//
// Reference: https://developer.apple.com/library/archive/documentation/DataManagement/Conceptual/CloudKitWebServicesReference/UploadAssetData.html
func (c *Client) UploadAsset(ctx context.Context, db Database, recordType, recordName, fieldName string, body []byte) (*AssetReceipt, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("upload asset: empty body")
	}

	// Step 1 — request an upload URL.
	tokenReq := map[string]any{
		"tokens": []any{
			map[string]any{
				"recordName": recordName,
				"recordType": recordType,
				"fieldName":  fieldName,
			},
		},
	}
	url := fmt.Sprintf("%s/%s/%s/%s/assets/upload",
		baseURL, c.container, c.environment, db)
	resp, err := c.do(ctx, http.MethodPost, url, tokenReq)
	if err != nil {
		return nil, fmt.Errorf("request upload URL: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("request upload URL: HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var tokenResp uploadTokensResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("decode upload tokens: %w", err)
	}
	if len(tokenResp.Tokens) == 0 || tokenResp.Tokens[0].URL == "" {
		return nil, fmt.Errorf("upload URL response missing token")
	}
	uploadURL := tokenResp.Tokens[0].URL

	// Step 2 — POST the bytes. CloudKit's upload URLs expect a multipart
	// form-data body with the asset under the "files" field, but the
	// CKWS implementation accepts a raw octet-stream POST too. We use
	// the simpler raw-stream form here.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upload PUT: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(body))

	uploadResp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload PUT: %w", err)
	}
	defer uploadResp.Body.Close()
	if uploadResp.StatusCode >= 400 {
		raw, _ := io.ReadAll(uploadResp.Body)
		return nil, fmt.Errorf("upload PUT: HTTP %d: %s", uploadResp.StatusCode, string(raw))
	}
	var receipt assetUploadResponse
	if err := json.NewDecoder(uploadResp.Body).Decode(&receipt); err != nil {
		return nil, fmt.Errorf("decode upload receipt: %w", err)
	}
	if receipt.SingleFile.Receipt == "" {
		return nil, fmt.Errorf("upload receipt missing")
	}
	return &AssetReceipt{
		FileChecksum:       receipt.SingleFile.FileChecksum,
		Size:               receipt.SingleFile.Size,
		WrappingKey:        receipt.SingleFile.WrappingKey,
		ReferenceChecksum:  receipt.SingleFile.ReferenceChecksum,
		Receipt:            receipt.SingleFile.Receipt,
	}, nil
}

// AssetReceipt is the bundle of values CloudKit returns from the upload
// URL — used to populate an ASSETID field on a subsequent records/modify.
type AssetReceipt struct {
	FileChecksum      string `json:"fileChecksum"`
	Size              int64  `json:"size"`
	WrappingKey       string `json:"wrappingKey"`
	ReferenceChecksum string `json:"referenceChecksum"`
	Receipt           string `json:"receipt"`
}

type uploadTokensResponse struct {
	Tokens []struct {
		RecordName string `json:"recordName"`
		FieldName  string `json:"fieldName"`
		URL        string `json:"url"`
	} `json:"tokens"`
}

type assetUploadResponse struct {
	SingleFile AssetReceipt `json:"singleFile"`
}
