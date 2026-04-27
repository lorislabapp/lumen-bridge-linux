// Package auth handles the per-user CloudKit Web Services sign-in flow.
//
// Apple's CloudKit JS uses a redirect-based sign-in: the daemon points
// the user at https://api.apple-cloudkit.com/database/1/{container}/{env}/users/caller?ckAPIToken=...
// which 302s to apple-id-sign-in if no session exists. After sign-in,
// Apple redirects to the registered redirect URI with `ckSession` in the
// query string. CloudKit JS apps register their domain with Apple; CLI
// daemons can't do that, so we use a different pattern:
//
//   1. We spin up a tiny localhost HTTP server on a free port.
//   2. We print a URL the user opens in any browser. The URL points to
//      a static helper page on lorislab.fr that runs CloudKit JS,
//      lets the user sign in, then displays the resulting session token
//      and tells the user to paste it back into the daemon's listener.
//   3. The daemon's HTTP server has one form: paste the ckSession token
//      → POST it back → daemon persists to ~/.config/lumen-bridge/token.json.
//
// This is a "device flow"-ish pattern (the user moves the credential by
// hand from one place to another) — slightly more friction than a pure
// redirect, but it works for any deployment topology including SSH-only
// NAS boxes where the daemon has no browser of its own.
//
// v0.0.1 / v0.1 ship the localhost listener + paste form; v0.2.0 will
// add a fully automated redirect flow once the static helper page is
// live at lorislab.fr/lumen-bridge/auth.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type StoredTokens struct {
	APIToken  string    `json:"api_token"`
	UserToken string    `json:"user_token"`
	UserID    string    `json:"user_id,omitempty"`
	IssuedAt  time.Time `json:"issued_at"`
}

// Load returns the persisted tokens or env-var fallbacks. Returns
// (nil, nil) when no tokens are available — the caller should then run
// the interactive sign-in flow.
func Load(path string) (*StoredTokens, error) {
	if envAPI := os.Getenv("LB_CK_API_TOKEN"); envAPI != "" {
		return &StoredTokens{
			APIToken:  envAPI,
			UserToken: os.Getenv("LB_CK_USER_TOKEN"),
			IssuedAt:  time.Now(),
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
	if t.IssuedAt.IsZero() {
		t.IssuedAt = time.Now()
	}
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}

// InteractiveOptions configures the sign-in flow.
type InteractiveOptions struct {
	APIToken    string // pre-supplied container API token (required for v0.1)
	OutputPath  string // ~/.config/lumen-bridge/token.json
	BindAddr    string // e.g. "127.0.0.1:0" — port 0 picks a free one
	Timeout     time.Duration
	HelperURL   string // helper page to direct the user to (templated with apiToken)
	NotifyReady func(localURL, helperURL string) // called once the listener is up
}

// Interactive runs the localhost-paste flow. It blocks until the user
// pastes a token in the form (or the timeout fires), persists, and
// returns. Cancelling ctx aborts the flow cleanly.
//
// The user's browser path:
//
//   1. Open helperURL on lorislab.fr → sign in to iCloud → see a token.
//   2. Open the printed local URL → paste the token → submit.
//
// The daemon stops the HTTP server as soon as the form posts.
func Interactive(ctx context.Context, opts InteractiveOptions) (*StoredTokens, error) {
	if opts.APIToken == "" {
		return nil, errors.New("APIToken is required (set via env LB_CK_API_TOKEN or generate one in CloudKit Dashboard)")
	}
	if opts.BindAddr == "" {
		opts.BindAddr = "127.0.0.1:0"
	}
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Minute
	}
	if opts.HelperURL == "" {
		opts.HelperURL = "https://lorislab.fr/lumen-bridge/auth"
	}

	listener, err := net.Listen("tcp", opts.BindAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", opts.BindAddr, err)
	}
	defer listener.Close()

	localURL := "http://" + listener.Addr().String() + "/"
	helperURL := opts.HelperURL + "?apiToken=" + opts.APIToken

	resultCh := make(chan *StoredTokens, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = pasteFormTpl.Execute(w, map[string]string{"HelperURL": helperURL})
	})
	mux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		token := strings.TrimSpace(r.FormValue("token"))
		if token == "" {
			http.Error(w, "token required", http.StatusBadRequest)
			return
		}
		stored := &StoredTokens{
			APIToken:  opts.APIToken,
			UserToken: token,
			IssuedAt:  time.Now(),
		}
		if err := Save(opts.OutputPath, stored); err != nil {
			errCh <- err
			http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(successHTML))
		resultCh <- stored
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if opts.NotifyReady != nil {
		opts.NotifyReady(localURL, helperURL)
	}

	timeout := time.After(opts.Timeout)
	select {
	case t := <-resultCh:
		return t, nil
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeout:
		return nil, fmt.Errorf("sign-in timed out after %s — re-run `lumen-bridge auth` to retry", opts.Timeout)
	}
}

var pasteFormTpl = template.Must(template.New("paste").Parse(`<!DOCTYPE html>
<html lang="en"><head>
  <meta charset="utf-8"><title>Lumen Bridge — Sign In</title>
  <style>
    body{font-family:system-ui,-apple-system,sans-serif;max-width:560px;margin:6em auto;padding:0 1em;line-height:1.55;color:#222}
    h1{font-size:1.4em}
    code{background:#f0f0f5;padding:0.1em 0.4em;border-radius:4px;font-size:0.95em}
    .step{border-left:3px solid #007aff;padding:0.4em 0 0.4em 1em;margin:1.2em 0;background:#fafafe}
    a{color:#007aff;font-weight:500}
    textarea{width:100%;min-height:5.5em;font-family:ui-monospace,monospace;padding:0.6em;border:1px solid #c8c8d4;border-radius:8px;font-size:14px}
    button{background:#007aff;color:white;border:0;padding:0.7em 1.6em;border-radius:8px;font-size:15px;cursor:pointer;margin-top:0.8em}
    button:hover{background:#0064d8}
  </style>
</head><body>
  <h1>Lumen Bridge — sign in to iCloud</h1>
  <p>This local helper window walks you through the one-time sign-in. It only runs while <code>lumen-bridge auth</code> is open; it can't be reached from outside this machine.</p>

  <div class="step"><b>Step 1.</b> Open the iCloud sign-in page in any browser:<br>
    <a href="{{.HelperURL}}" target="_blank" rel="noopener">{{.HelperURL}}</a></div>

  <div class="step"><b>Step 2.</b> Sign in with your Apple ID. The page will display a <code>ckSession</code> token after sign-in.</div>

  <div class="step"><b>Step 3.</b> Paste the token below and submit. The daemon stores it locally (<code>~/.config/lumen-bridge/token.json</code>, mode 0600) and never transmits it to anyone but Apple.</div>

  <form action="/submit" method="POST">
    <textarea name="token" placeholder="ckSession token from Step 2…" autofocus required></textarea>
    <button type="submit">Save and continue</button>
  </form>
</body></html>`))

const successHTML = `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Done</title>
<style>body{font-family:system-ui;text-align:center;margin:8em}h1{color:#28a745}</style>
</head><body><h1>✓ Token saved</h1><p>You can close this tab and run <code>lumen-bridge run</code>.</p></body></html>`
