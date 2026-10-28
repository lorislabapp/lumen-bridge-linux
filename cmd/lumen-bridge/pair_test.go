package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/hkdf"
)

// encryptLikeWorker reproduces encryptWithPairCode() from the deployed
// lumen-bridge-relay Worker: HKDF-SHA256(code, salt "lumen-bridge-v1-mvp",
// empty info) → AES-256-GCM, output base64(nonce12 || ciphertext+tag).
func encryptLikeWorker(t *testing.T, code, plaintext string) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, []byte(code), []byte("lumen-bridge-v1-mvp"), nil), key); err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	return base64.StdEncoding.EncodeToString(append(nonce, gcm.Seal(nil, nonce, []byte(plaintext), nil)...))
}

func TestPairingTokenRoundTrip(t *testing.T) {
	envelope := `{"token":"secret-token","id":"row-1","user_ref":"lumen-user-abcd1234","scope":"bridge"}`
	ct := encryptLikeWorker(t, "123456", envelope)
	plain, err := decryptPairingToken(ct, "123456")
	if err != nil || plain != envelope {
		t.Fatalf("decrypt: %q %v", plain, err)
	}
	if _, err := decryptPairingToken(ct, "654321"); err == nil {
		t.Fatal("wrong code must fail")
	}

	path := filepath.Join(t.TempDir(), "sub", "device-token.json")
	if err := savePairingToken(plain, path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file perms: %v %v", info, err)
	}
	raw, _ := os.ReadFile(path)
	var got map[string]string
	_ = json.Unmarshal(raw, &got)
	if got["token"] != "secret-token" || got["user_ref"] != "lumen-user-abcd1234" {
		t.Fatalf("saved: %v", got)
	}
}

func TestPairingWebSocketURL(t *testing.T) {
	cases := map[string]string{
		"https://relay.lorislab.fr":  "wss://relay.lorislab.fr/pair/ws/123456",
		"https://relay.lorislab.fr/": "wss://relay.lorislab.fr/pair/ws/123456",
		"wss://relay.lorislab.fr":    "wss://relay.lorislab.fr/pair/ws/123456",
		"http://localhost:8787":      "ws://localhost:8787/pair/ws/123456",
	}
	for in, want := range cases {
		got, err := pairingWebSocketURL(in, "123456")
		if err != nil || got != want {
			t.Errorf("%s → %q (%v), want %q", in, got, err, want)
		}
	}
	if _, err := pairingWebSocketURL("ftp://x", "123456"); err == nil {
		t.Error("ftp scheme must be rejected")
	}
	if !isSixDigits("012345") || isSixDigits("12345a") || isSixDigits("1234567") {
		t.Error("isSixDigits")
	}
}
