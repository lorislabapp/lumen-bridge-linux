package frigate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/events/1.2-abc/snapshot.jpg":
			_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF})
		case "/api/events/1.2-abc/clip.mp4":
			http.NotFound(w, r)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := New(srv.URL + "/")
	b, err := c.FetchSnapshot(context.Background(), "1.2-abc")
	if err != nil || len(b) != 3 {
		t.Fatalf("snapshot: %v %v", b, err)
	}
	b, err = c.FetchClip(context.Background(), "1.2-abc")
	if err != nil || b != nil {
		t.Fatalf("missing clip should be (nil, nil), got %v %v", b, err)
	}
	if _, err := c.FetchClip(context.Background(), "other"); err == nil {
		t.Fatal("5xx should be an error")
	}
}
