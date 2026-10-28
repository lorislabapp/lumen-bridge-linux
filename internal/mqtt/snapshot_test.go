package mqtt

import (
	"testing"
	"time"
)

func TestSnapshotFresh_IgnoresRetainedReplay(t *testing.T) {
	c := NewSnapshotCache(time.Hour)
	now := time.Now()
	c.store("frigate/front/person/snapshot", []byte{0xFF, 0xD8}, true, now)
	if got := c.Fresh("front", "person", now.Add(-time.Minute)); got != nil {
		t.Fatal("retained snapshot must not be attached to a new event")
	}
	if got := c.Latest("front", "person"); got == nil {
		t.Fatal("Latest should still expose the retained snapshot")
	}
}

func TestSnapshotFresh_RequiresSinceWindow(t *testing.T) {
	c := NewSnapshotCache(time.Hour)
	now := time.Now()
	c.store("frigate/front/person/snapshot", []byte{1}, false, now.Add(-30*time.Second))
	if got := c.Fresh("front", "person", now.Add(-5*time.Second)); got != nil {
		t.Fatal("snapshot older than the event window must be ignored")
	}
	c.store("frigate/front/person/snapshot", []byte{2}, false, now)
	got := c.Fresh("front", "person", now.Add(-5*time.Second))
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("expected fresh snapshot, got %v", got)
	}
	if c.Fresh("front", "car", now.Add(-5*time.Second)) != nil {
		t.Fatal("label mismatch must return nil")
	}
}

func TestSnapshotStore_IgnoresBadTopics(t *testing.T) {
	c := NewSnapshotCache(time.Hour)
	c.store("frigate/events", []byte{1}, false, time.Now())
	c.store("frigate/front/person/snapshot", nil, false, time.Now())
	if len(c.entries) != 0 {
		t.Fatalf("unexpected entries: %d", len(c.entries))
	}
}
