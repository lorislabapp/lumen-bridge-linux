// Package auth handles the per-user CloudKit Web Services sign-in flow.
//
// The flow is the same one CloudKit JS uses in browsers:
//   1. Daemon hits POST .../users/caller with the container API token.
//   2. CloudKit replies with a redirect URL ("ckSession-acquire URL").
//   3. The user opens that URL, signs in to Apple ID, approves access.
//   4. Apple redirects back with a session token, which the daemon stores
//      to ~/.config/lumen-bridge/token.json (file mode 0600).
//
// v0.0.1: this package is a stub — we currently expect both the API token
// and the user token to be supplied via env vars (LB_CK_API_TOKEN,
// LB_CK_USER_TOKEN) so the rest of the daemon can be exercised end-to-end
// while the web flow is implemented in v0.2.0.
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type StoredTokens struct {
	APIToken  string `json:"api_token"`
	UserToken string `json:"user_token"`
	UserID    string `json:"user_id,omitempty"`
}

// Load returns the persisted tokens or env-var fallbacks. Returns
// (nil, nil) when no tokens are available — the caller should then run
// the interactive sign-in flow.
func Load(path string) (*StoredTokens, error) {
	if envAPI := os.Getenv("LB_CK_API_TOKEN"); envAPI != "" {
		return &StoredTokens{
			APIToken:  envAPI,
			UserToken: os.Getenv("LB_CK_USER_TOKEN"),
		}, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read tokens: %w", err)
	}
	var t StoredTokens
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("parse tokens: %w", err)
	}
	if t.APIToken == "" {
		return nil, fmt.Errorf("tokens file missing api_token")
	}
	return &t, nil
}

// Save persists the token bundle with strict file permissions. The
// containing directory is created if missing (mode 0700).
func Save(path string, t *StoredTokens) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("mkdir tokens dir: %w", err)
	}
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}

// Interactive prints the Apple sign-in URL and blocks until the user
// completes the web flow. Returns the user token to be persisted.
//
// v0.0.1: this is a placeholder. The real implementation will:
//   1. POST to /users/caller to get a sign-in URL
//   2. Print the URL and instructions
//   3. Spin up a local HTTP server on a random port to receive the redirect
//   4. Block on the redirect, extract the session token from the query,
//      shut down the server, return.
func Interactive(_ string) (*StoredTokens, error) {
	return nil, fmt.Errorf("interactive sign-in flow not yet implemented (v0.2.0); set LB_CK_API_TOKEN and LB_CK_USER_TOKEN env vars instead")
}
