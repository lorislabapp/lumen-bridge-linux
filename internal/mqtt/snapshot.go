package mqtt

import (
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// SnapshotCache stores the latest retained snapshot per (camera, label)
// pair, as published by Frigate on `frigate/{camera}/{label}/snapshot`.
// When a "new" event arrives, the bridge looks up the matching snapshot
// in this cache and uploads it as a CKAsset alongside the FrigateEvent
// record.
//
// Frigate retains the snapshot on the broker, so a fresh subscriber sees
// the most recent JPEG immediately on connect — but we still expire
// entries after a TTL so a long-lived bridge doesn't accumulate stale
// images for cameras that have been removed from Frigate's config.
type SnapshotCache struct {
	mu      sync.RWMutex
	entries map[string]snapshotEntry
	ttl     time.Duration
}

type snapshotEntry struct {
	bytes    []byte
	storedAt time.Time
	// retained is true when the broker replayed the JPEG on subscribe.
	// Its age is unknown (it may belong to an event from hours ago), so
	// Fresh never returns it.
	retained bool
}

func NewSnapshotCache(ttl time.Duration) *SnapshotCache {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &SnapshotCache{
		entries: make(map[string]snapshotEntry),
		ttl:     ttl,
	}
}

// Subscribe attaches the cache to a connected paho.Client by subscribing
// to the wildcard snapshot topic. Idempotent — calling twice on the same
// client just resubscribes (paho handles the dedupe).
func (s *SnapshotCache) Subscribe(client paho.Client, topicPrefix string) error {
	wildcard := topicPrefix + "/+/+/snapshot"
	tok := client.Subscribe(wildcard, 0, s.handleMessage)
	tok.Wait()
	return tok.Error()
}

func (s *SnapshotCache) handleMessage(_ paho.Client, msg paho.Message) {
	s.store(msg.Topic(), msg.Payload(), msg.Retained(), time.Now())
}

func (s *SnapshotCache) store(topic string, payload []byte, retained bool, now time.Time) {
	parts := strings.Split(topic, "/")
	// Expected: [topicPrefix, camera, label, "snapshot"] — at least 4 parts.
	if len(parts) < 4 || parts[len(parts)-1] != "snapshot" || len(payload) == 0 {
		return
	}
	camera := parts[len(parts)-3]
	label := parts[len(parts)-2]
	key := camera + "|" + label
	bytes := append([]byte(nil), payload...) // copy — paho may reuse the buffer
	s.mu.Lock()
	s.entries[key] = snapshotEntry{bytes: bytes, storedAt: now, retained: retained}
	s.mu.Unlock()
}

// Fresh returns the snapshot for (camera, label) only if it was received
// live (not a retained replay) at or after `since`. This is what the
// bridge attaches to a new event: a JPEG from a previous detection on
// the same camera/label must never be shown as this event's thumbnail.
func (s *SnapshotCache) Fresh(camera, label string, since time.Time) []byte {
	key := camera + "|" + label
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[key]
	if !ok || e.retained || e.storedAt.Before(since) || time.Since(e.storedAt) > s.ttl {
		return nil
	}
	return e.bytes
}

// Latest returns the most recently retained snapshot for (camera, label),
// or nil if none / the entry has expired.
func (s *SnapshotCache) Latest(camera, label string) []byte {
	key := camera + "|" + label
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[key]
	if !ok {
		return nil
	}
	if time.Since(e.storedAt) > s.ttl {
		return nil
	}
	return e.bytes
}

// Sweep drops expired entries to keep memory bounded. Should be called
// periodically (e.g. every 5 minutes) by a goroutine in main().
func (s *SnapshotCache) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	dropped := 0
	cutoff := time.Now().Add(-s.ttl)
	for k, e := range s.entries {
		if e.storedAt.Before(cutoff) {
			delete(s.entries, k)
			dropped++
		}
	}
	return dropped
}
