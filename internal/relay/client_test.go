package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Worker (lumen-bridge-relay, deployed 2026-10-27) accepts exactly
// these shapes; keep the client in lockstep.
func TestPostEvent_WireContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events/ingest" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header: %q", r.Header.Get("Authorization"))
		}
		var body struct {
			Event    map[string]any `json:"event"`
			Snapshot map[string]any `json:"snapshot"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, k := range []string{"id", "camera", "label", "detectedAt", "topScore", "zones"} {
			if _, ok := body.Event[k]; !ok {
				t.Errorf("event missing %q", k)
			}
		}
		if len(body.Event) != 6 {
			t.Errorf("event must carry only the 6 allow-listed fields, got %v", body.Event)
		}
		if body.Snapshot["receipt"] != "r" || body.Snapshot["fileChecksum"] != "c" {
			t.Errorf("snapshot receipt: %v", body.Snapshot)
		}
		_, _ = io.WriteString(w, `{"recordName":"frigate-1.2-a","userRef":"lumen-user-x"}`)
	}))
	defer srv.Close()
	c, err := New(Options{RelayURL: srv.URL + "/", DeviceToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.PostEvent(context.Background(),
		Event{ID: "1.2-a", Camera: "front", Label: "person", DetectedAt: 1, TopScore: 0.5, Zones: []string{}},
		&AssetReceipt{FileChecksum: "c", Size: 3, Receipt: "r"})
	if err != nil || resp.RecordName != "frigate-1.2-a" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestAssetFlow_WireContract(t *testing.T) {
	var cdn *httptest.Server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/assets/request-upload":
			if body["fieldName"] != "clip" || body["frigateID"] != "1.2-a" {
				t.Errorf("request-upload body: %v", body)
			}
			_, _ = io.WriteString(w, `{"url":"`+cdn.URL+`/up","recordName":"frigate-1.2-a","fieldName":"clip"}`)
		case "/events/update-asset":
			asset, _ := body["asset"].(map[string]any)
			if body["recordName"] != "frigate-1.2-a" || body["fieldName"] != "clip" || asset["receipt"] != "r" {
				t.Errorf("update-asset body: %v", body)
			}
			_, _ = io.WriteString(w, `{"recordName":"frigate-1.2-a"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cdn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data; boundary=") {
			t.Errorf("content-type: %q", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("device token must never be sent to the CDN")
		}
		_, _ = io.WriteString(w, `{"singleFile":{"fileChecksum":"c","size":3,"receipt":"r"}}`)
	}))
	defer cdn.Close()

	c, _ := New(Options{RelayURL: srv.URL, DeviceToken: "tok"})
	up, err := c.RequestAssetUpload(context.Background(), AssetFieldClip, "1.2-a")
	if err != nil {
		t.Fatal(err)
	}
	rc, err := c.UploadAsset(context.Background(), up.URL, []byte{1, 2, 3}, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateEventAsset(context.Background(), up.RecordName, AssetFieldClip, rc); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RequestAssetUpload(context.Background(), "thumbnail", "x"); err == nil {
		t.Fatal("unknown asset field must be rejected client-side")
	}
}

func TestRetryable(t *testing.T) {
	cases := map[error]bool{
		&HTTPError{Status: 503}:         true,
		&HTTPError{Status: 502}:         true,
		&HTTPError{Status: 429}:         true,
		&HTTPError{Status: 401}:         false,
		&HTTPError{Status: 400}:         false,
		errors.New("dial tcp: refused"): true,
		context.Canceled:                false,
		nil:                             false,
	}
	for err, want := range cases {
		if got := Retryable(err); got != want {
			t.Errorf("Retryable(%v) = %v want %v", err, got, want)
		}
	}
}
