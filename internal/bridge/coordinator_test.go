package bridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lorislabapp/lumen-bridge-linux/internal/mqtt"
	"github.com/lorislabapp/lumen-bridge-linux/internal/relay"
)

type fakeRelay struct {
	mu          sync.Mutex
	posts       []relay.Event
	snapshots   []*relay.AssetReceipt
	uploads     []string // fieldName per upload request
	updates     []string // recordName:field
	failPosts   int      // first N posts fail with 503
	failPostsAs int
}

func (f *fakeRelay) PostEvent(_ context.Context, e relay.Event, s *relay.AssetReceipt) (*relay.PostEventResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPosts > 0 {
		f.failPosts--
		return nil, &relay.HTTPError{Endpoint: "/events/ingest", Status: f.failPostsAs}
	}
	f.posts = append(f.posts, e)
	f.snapshots = append(f.snapshots, s)
	return &relay.PostEventResponse{RecordName: "frigate-" + e.ID}, nil
}

func (f *fakeRelay) RequestAssetUpload(_ context.Context, field, id string) (*relay.RequestUploadResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, field)
	return &relay.RequestUploadResponse{URL: "https://cdn.example/up", RecordName: "frigate-" + id, FieldName: field}, nil
}

func (f *fakeRelay) UploadAsset(_ context.Context, _ string, p []byte, _ string) (*relay.AssetReceipt, error) {
	return &relay.AssetReceipt{FileChecksum: "c", Size: int64(len(p)), Receipt: "r"}, nil
}

func (f *fakeRelay) UpdateEventAsset(_ context.Context, rn, field string, _ *relay.AssetReceipt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, rn+":"+field)
	return nil
}

func (f *fakeRelay) snapshot() (posts []relay.Event, snaps []*relay.AssetReceipt, uploads, updates []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]relay.Event(nil), f.posts...), append([]*relay.AssetReceipt(nil), f.snapshots...),
		append([]string(nil), f.uploads...), append([]string(nil), f.updates...)
}

type fakeFrigate struct {
	snapshot []byte
	clip     []byte
}

func (f *fakeFrigate) FetchSnapshot(context.Context, string) ([]byte, error) { return f.snapshot, nil }
func (f *fakeFrigate) FetchClip(context.Context, string) ([]byte, error)     { return f.clip, nil }

type fakeMQTT struct {
	ready   chan struct{}
	handler mqtt.Handler
}

func (m *fakeMQTT) Connect(_ context.Context, h mqtt.Handler) error {
	m.handler = h
	close(m.ready)
	return nil
}
func (m *fakeMQTT) Disconnect() {}

func start(t *testing.T, opts Options) (*Coordinator, *fakeMQTT, context.CancelFunc) {
	t.Helper()
	m := &fakeMQTT{ready: make(chan struct{})}
	opts.MQTT = m
	opts.RetryBaseDelay = time.Millisecond
	opts.SnapshotWait = 10 * time.Millisecond
	c := New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	<-m.ready
	return c, m, cancel
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func ev(phase mqtt.EventPhase) mqtt.Event {
	return mqtt.Event{ID: "1.2-a", Camera: "front", Label: "person", Zones: []string{"porch"},
		TopScore: 0.9, StartTime: time.Now(), Phase: phase, HasClip: phase == mqtt.PhaseEnd}
}

func TestNewEventForwardedWithSnapshot_AndClipBackfilledOnEnd(t *testing.T) {
	r := &fakeRelay{}
	c, m, _ := start(t, Options{Relay: r, Frigate: &fakeFrigate{snapshot: []byte{0xFF}, clip: []byte{1, 2, 3}}})
	m.handler(context.Background(), ev(mqtt.PhaseNew))
	m.handler(context.Background(), ev(mqtt.PhaseUpdate))
	m.handler(context.Background(), ev(mqtt.PhaseEnd))
	waitFor(t, func() bool { return c.Stats().ClipUploads == 1 })
	posts, snaps, uploads, updates := r.snapshot()
	if len(posts) != 1 || posts[0].ID != "1.2-a" || posts[0].Zones[0] != "porch" {
		t.Fatalf("posts: %+v", posts)
	}
	if snaps[0] == nil {
		t.Fatal("snapshot receipt should be attached to the new event")
	}
	if len(uploads) != 2 || uploads[0] != "snapshot" || uploads[1] != "clip" {
		t.Fatalf("uploads: %v", uploads)
	}
	if len(updates) != 1 || updates[0] != "frigate-1.2-a:clip" {
		t.Fatalf("updates: %v", updates)
	}
	if s := c.Stats(); s.Received != 3 || s.Forwarded != 1 || s.Skipped != 1 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestTransientRelayErrorIsRetried(t *testing.T) {
	r := &fakeRelay{failPosts: 2, failPostsAs: 503}
	c, m, _ := start(t, Options{Relay: r})
	m.handler(context.Background(), ev(mqtt.PhaseNew))
	waitFor(t, func() bool { return c.Stats().Forwarded == 1 })
	if c.Stats().Errors != 0 {
		t.Fatalf("errors: %+v", c.Stats())
	}
}

func TestPermanentRelayErrorIsNotRetried(t *testing.T) {
	r := &fakeRelay{failPosts: 1, failPostsAs: 401}
	c, m, _ := start(t, Options{Relay: r})
	m.handler(context.Background(), ev(mqtt.PhaseNew))
	waitFor(t, func() bool { return c.Stats().Errors == 1 })
	posts, _, _, _ := r.snapshot()
	r.mu.Lock()
	left := r.failPosts
	r.mu.Unlock()
	if len(posts) != 0 || left != 0 {
		t.Fatalf("401 must not be retried: posts=%d", len(posts))
	}
}

func TestStaleRetainedSnapshotIsNotAttached(t *testing.T) {
	r := &fakeRelay{}
	cache := mqtt.NewSnapshotCache(time.Hour)
	c, m, _ := start(t, Options{Relay: r, Snapshots: cache})
	m.handler(context.Background(), ev(mqtt.PhaseNew)) // cache empty → no snapshot
	waitFor(t, func() bool { return c.Stats().Forwarded == 1 })
	_, snaps, uploads, _ := r.snapshot()
	if snaps[0] != nil || len(uploads) != 0 {
		t.Fatal("no fresh snapshot → event must ship metadata-only")
	}
}

func TestEndWithoutRecordedNewSkipsClip(t *testing.T) {
	r := &fakeRelay{}
	c, m, _ := start(t, Options{Relay: r, Frigate: &fakeFrigate{clip: []byte{1}}})
	m.handler(context.Background(), ev(mqtt.PhaseEnd))
	waitFor(t, func() bool { return c.Stats().Skipped == 1 })
	_, _, uploads, updates := r.snapshot()
	if len(uploads) != 0 || len(updates) != 0 {
		t.Fatal("clip must not be uploaded for an event this bridge never recorded")
	}
}

func TestDryRunNeverCallsRelay(t *testing.T) {
	c, m, _ := start(t, Options{})
	m.handler(context.Background(), ev(mqtt.PhaseNew))
	m.handler(context.Background(), ev(mqtt.PhaseEnd))
	waitFor(t, func() bool { return c.Stats().Skipped == 2 })
}
