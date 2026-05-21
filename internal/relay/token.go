package relay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// StoredDeviceToken is the on-disk shape produced by `lumen-bridge pair`
// (or written by hand during bootstrap). The file lives at the path in
// config.relay.device_token_path, mode 0600.
//
// We deliberately keep the schema narrow: just the bearer token plus
// metadata. The relay owns rotation; the daemon never refreshes the
// token on its own.
type StoredDeviceToken struct {
	Token  string `json:"token"`
	ID     string `json:"id,omitempty"`      // server-side row id (useful for self-revoke)
	UserRef string `json:"user_ref,omitempty"` // useful for /metrics labels
	Scope  string `json:"scope,omitempty"`   // expected: "bridge"
}

// LoadDeviceToken reads a token file. Returns (nil, nil) when the file
// doesn't exist — callers should then prompt the user to run
// `lumen-bridge pair`. Returns the env-var override LB_RELAY_DEVICE_TOKEN
// when set, so container deployments can inject the token without a
// disk file.
func LoadDeviceToken(path string) (*StoredDeviceToken, error) {
	if envToken := os.Getenv("LB_RELAY_DEVICE_TOKEN"); envToken != "" {
		return &StoredDeviceToken{
			Token: envToken,
			Scope: "bridge",
		}, nil
	}
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var t StoredDeviceToken
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if t.Token == "" {
		return nil, fmt.Errorf("%s: missing 'token' field", path)
	}
	return &t, nil
}

// SaveDeviceToken persists the token to disk, mode 0600. The directory
// is created with 0700 if missing.
func SaveDeviceToken(path string, t StoredDeviceToken) error {
	if path == "" {
		return fmt.Errorf("empty path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return err
	}
	return nil
}
