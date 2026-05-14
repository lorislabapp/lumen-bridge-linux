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
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/hkdf"
)

func pairCmd(args []string) {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	code := fs.String("code", "", "6-digit pairing code from app (required)")
	relayURL := fs.String("relay", "wss://relay.lorislab.fr", "Relay server URL")
	fs.Parse(args)

	if *code == "" {
		fmt.Fprintln(os.Stderr, "Error: --code flag is required")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Usage: lumen-bridge pair --code <6-digit-code>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Pair this Bridge with your Lumen app to receive CloudKit credentials.")
		fmt.Fprintln(os.Stderr, "The app will display a 6-digit pairing code.")
		os.Exit(1)
	}

	if len(*code) != 6 {
		fmt.Fprintln(os.Stderr, "Error: pairing code must be exactly 6 digits")
		os.Exit(1)
	}

	if err := runPairing(*code, *relayURL); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runPairing(code string, relayBaseURL string) error {
	// Build WebSocket URL
	wsURL, err := url.Parse(relayBaseURL)
	if err != nil {
		return fmt.Errorf("invalid relay URL: %w", err)
	}
	wsURL.Path = fmt.Sprintf("/pair/ws/%s", code)

	fmt.Printf("🔗 Connecting to relay...\n")

	// Connect to relay WebSocket
	conn, _, err := websocket.DefaultDialer.Dial(wsURL.String(), nil)
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
			if err := savePairingToken(token); err != nil {
				return fmt.Errorf("failed to save token: %w", err)
			}

			// Confirm to relay
			confirmMsg := map[string]string{"type": "confirmed"}
			if err := conn.WriteJSON(confirmMsg); err != nil {
				// Non-fatal, token is already saved
				fmt.Printf("⚠️  Warning: failed to confirm to relay: %v\n", err)
			}

			fmt.Printf("\n✅ Token received and saved to ~/.config/lumen-bridge/token.json\n")
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

// savePairingToken saves the CloudKit token to config file
func savePairingToken(token string) error {
	// Create config directory
	configDir := filepath.Join(os.Getenv("HOME"), ".config", "lumen-bridge")
	if configDir == "/.config/lumen-bridge" {
		// Fallback if HOME not set (running as root)
		configDir = "/root/.config/lumen-bridge"
	}

	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("failed to create config dir: %w", err)
	}

	// Prepare token JSON
	tokenJSON := map[string]string{
		"ckSession": token,
	}

	data, err := json.MarshalIndent(tokenJSON, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal token: %w", err)
	}

	// Write to file
	tokenFile := filepath.Join(configDir, "token.json")
	if err := os.WriteFile(tokenFile, data, 0600); err != nil {
		return fmt.Errorf("failed to write token file: %w", err)
	}

	return nil
}
