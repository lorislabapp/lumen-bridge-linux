package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/hkdf"

	"github.com/lorislabapp/lumen-bridge-linux/internal/config"
)

func pairCmd(args []string) {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	code := fs.String("code", "", "6-digit pairing code shown by the Lumen app (required)")
	relayURL := fs.String("relay", "", "relay URL (default: relay.url from config, else https://relay.lorislab.fr)")
	cfgPath := fs.String("config", "", "config.yaml to read relay.url and relay.device_token_path from")
	tokenPath := fs.String("token-path", "", "where to write the device token (default: relay.device_token_path)")
	_ = fs.Parse(args)

	if *code == "" {
		fmt.Fprintln(os.Stderr, "Error: --code flag is required")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Usage: lumen-bridge pair --code <6-digit-code> [--config PATH] [--token-path PATH]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "In the Lumen app: Settings → Paired Bridges → Pair a Bridge shows the code.")
		os.Exit(1)
	}
	if !isSixDigits(*code) {
		fmt.Fprintln(os.Stderr, "Error: pairing code must be exactly 6 digits")
		os.Exit(1)
	}

	// Resolve relay URL + token path: flag > config/env > defaults.
	// A config that fails validation (e.g. mqtt.host not filled in yet)
	// must not block pairing, so fall back to defaults + env.
	resolvedRelay := config.DefaultRelayURL
	resolvedToken := config.DefaultDeviceTokenPath()
	if cfg, err := config.Load(*cfgPath); err == nil {
		resolvedRelay = cfg.Relay.URL
		resolvedToken = cfg.Relay.DeviceTokenPath
	} else {
		if v := os.Getenv("LB_RELAY_URL"); v != "" {
			resolvedRelay = v
		}
		if v := os.Getenv("LB_RELAY_DEVICE_TOKEN_PATH"); v != "" {
			resolvedToken = config.ExpandHome(v)
		}
	}
	if *relayURL != "" {
		resolvedRelay = *relayURL
	}
	if *tokenPath != "" {
		resolvedToken = config.ExpandHome(*tokenPath)
	}

	if err := runPairing(*code, resolvedRelay, resolvedToken); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func isSixDigits(s string) bool {
	if len(s) != 6 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// pairingWebSocketURL turns the relay base URL (https://… or wss://…)
// into the pairing WebSocket endpoint for a code.
func pairingWebSocketURL(relayBaseURL, code string) (string, error) {
	u, err := url.Parse(relayBaseURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid relay URL %q", relayBaseURL)
	}
	switch u.Scheme {
	case "https", "wss":
		u.Scheme = "wss"
	case "http", "ws":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("unsupported relay URL scheme %q", u.Scheme)
	}
	u.Path = "/pair/ws/" + code
	u.RawQuery = ""
	return u.String(), nil
}

func runPairing(code, relayBaseURL, tokenPath string) error {
	wsURL, err := pairingWebSocketURL(relayBaseURL, code)
	if err != nil {
		return err
	}

	fmt.Printf("🔗 Connecting to relay...\n")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to relay: %w\nMake sure the pairing code is correct and not expired", err)
	}
	defer conn.Close()

	fmt.Printf("⏳ Waiting for app to confirm...\n")

	// Set read deadline (5 minutes max)
	conn.SetReadDeadline(time.Now().Add(5 * time.Minute))

	// Wait for token message
	for {
		var msg struct {
			Type            string `json:"type"`
			EncryptedToken  string `json:"encrypted_token"`
			EphemeralPubkey string `json:"ephemeral_pubkey"`
		}

		if err := conn.ReadJSON(&msg); err != nil {
			return fmt.Errorf("connection closed: %w", err)
		}

		switch msg.Type {
		case "waiting":
			// Still waiting for app
			continue

		case "token":
			// Received encrypted token
			fmt.Printf("📦 Received encrypted token from app\n")

			// Decrypt token
			token, err := decryptPairingToken(msg.EncryptedToken, code)
			if err != nil {
				return fmt.Errorf("failed to decrypt token: %w", err)
			}

			// Save to config file
			if err := savePairingToken(token, tokenPath); err != nil {
				return fmt.Errorf("failed to save token: %w", err)
			}

			// Confirm to relay
			confirmMsg := map[string]string{"type": "confirmed"}
			if err := conn.WriteJSON(confirmMsg); err != nil {
				// Non-fatal, token is already saved
				fmt.Printf("⚠️  Warning: failed to confirm to relay: %v\n", err)
			}

			fmt.Printf("\n✅ Device token received and saved to %s\n", tokenPath)
			fmt.Printf("✅ Bridge is ready\n\n")
			fmt.Printf("Next steps:\n")
			fmt.Printf("  1. Restart the bridge: systemctl restart lumen-bridge\n")
			fmt.Printf("  2. Check logs: journalctl -u lumen-bridge -f\n\n")

			return nil

		default:
			fmt.Printf("⚠️  Unknown message type: %s\n", msg.Type)
		}
	}
}

// decryptPairingToken decrypts the token using code-derived key (MVP version)
func decryptPairingToken(encryptedB64 string, code string) (string, error) {
	// 1. Derive key from code (same as app)
	salt := []byte("lumen-bridge-v1-mvp")
	key := make([]byte, 32)

	kdf := hkdf.New(sha256.New, []byte(code), salt, nil)
	if _, err := io.ReadFull(kdf, key); err != nil {
		return "", fmt.Errorf("key derivation failed: %w", err)
	}

	// 2. Decode base64
	ciphertext, err := base64.StdEncoding.DecodeString(encryptedB64)
	if err != nil {
		return "", fmt.Errorf("base64 decode failed: %w", err)
	}

	// 3. Decrypt AES-GCM
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("cipher creation failed: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("GCM creation failed: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decryption failed: %w", err)
	}

	return string(plaintext), nil
}

// savePairingToken persists the device token received from the iOS app
// via the pair-relay WebSocket. As of v0.3.0 the payload is the
// relay-issued bearer token (NOT a CloudKit ckSession); the iOS app
// shapes it as a JSON envelope so we can carry the row id + user_ref +
// scope alongside the raw secret.
//
// On-disk format matches relay.StoredDeviceToken so the daemon's
// `relay.LoadDeviceToken` reads it back unmodified.
//
// Envelope shape we expect after decryption:
//
//	{ "token": "<base64url>", "id": "<uuid>", "user_ref": "lumen-user-...", "scope": "bridge" }
//
// If the payload is a bare string (older app builds), we wrap it
// ourselves with sensible defaults so the bridge still starts.
func savePairingToken(payload, tokenFile string) error {
	if tokenFile == "" {
		return fmt.Errorf("no token path")
	}
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		return fmt.Errorf("failed to create token dir: %w", err)
	}

	type envelope struct {
		Token   string `json:"token"`
		ID      string `json:"id,omitempty"`
		UserRef string `json:"user_ref,omitempty"`
		Scope   string `json:"scope,omitempty"`
	}

	var env envelope
	trimmed := strings.TrimSpace(payload)
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
			return fmt.Errorf("decode token envelope: %w", err)
		}
		if env.Token == "" {
			return fmt.Errorf("token envelope missing 'token' field")
		}
	} else {
		// Bare string fallback — wrap as bridge-scoped device token.
		env = envelope{Token: trimmed, Scope: "bridge"}
	}

	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	if err := os.WriteFile(tokenFile, data, 0o600); err != nil {
		return fmt.Errorf("failed to write token file: %w", err)
	}
	return nil
}
