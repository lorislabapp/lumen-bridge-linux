// Package bridge wires together the MQTT subscriber and the relay HTTP
// client that forwards Frigate events to the Lumen Bridge Relay Worker.
// The Coordinator owns the receive/forward counters and the per-event
// pipeline.
//
// The Worker holds the only CloudKit Server-to-Server key and writes a
// FrigateEvent record per event; the Lumen apps receive it through their
// CloudKit subscription. The bridge authenticates with the per-user
// device token from the pair flow.
//
// Work never runs on the MQTT receive goroutine: decoded events go into
// a bounded queue drained by one worker (so "new" is always posted
// before the matching "end" is handled), and slow clip backfills go into
// a second queue so a large MP4 upload never delays the next alert.
package bridge

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lorislabapp/lumen-bridge-linux/internal/mqtt"
	"github.com/lorislabapp/lumen-bridge-linux/internal/relay"
)

// EventSource is the MQTT side (satisfied by *mqtt.Client).
type EventSource interface {
	Connect(ctx context.Context, onEvent mqtt.Handler) error
	Disconnect()
}

// Relay is the Worker side (satisfied by *relay.Client).
type Relay interface {
	PostEvent(ctx context.Context, e relay.Event, snapshot *relay.AssetReceipt) (*relay.PostEventResponse, error)
	RequestAssetUpload(ctx context.Context, fieldName, frigateID string) (*relay.RequestUploadResponse, error)
	UploadAsset(ctx context.Context, uploadURL string, payload []byte, contentType string) (*relay.AssetReceipt, error)
	UpdateEventAsset(ctx context.Context, recordName, fieldName string, asset *relay.AssetReceipt) error
}

// Frigate is the optional Frigate HTTP API (satisfied by *frigate.Client).
type Frigate interface {
	FetchSnapshot(ctx context.Context, eventID string) ([]byte, error)
	FetchClip(ctx context.Context, eventID string) ([]byte, error)
}

type Options struct {
	MQTT      EventSource
	Relay     Relay               // nil = dry-run, decode + log only
	Snapshots *mqtt.SnapshotCache // optional — live MQTT snapshots
	Frigate   Frigate             // optional — HTTP snapshot + clip backfill
	Logger    *slog.Logger

	// Tuning; zero values pick production defaults.
	QueueSize      int
	RetryAttempts  int
	RetryBaseDelay time.Duration
	SnapshotWait   time.Duration
}

type Coordinator struct {
	mqtt      EventSource
	relay     Relay
	snapshots *mqtt.SnapshotCache
	frigate   Frigate
	logger    *slog.Logger

	events chan mqtt.Event
	clips  chan clipJob

	retryAttempts  int
	retryBaseDelay time.Duration
	snapshotWait   time.Duration

	receivedCount   atomic.Int64
	forwardedCount  atomic.Int64
	skippedCount    atomic.Int64
	droppedCount    atomic.Int64
	errorCount      atomic.Int64
	snapshotUploads atomic.Int64
	clipUploads     atomic.Int64

	mu       sync.Mutex
	recorded map[string]string // frigate id → recordName, for clip backfill
}

type clipJob struct {
	ev         mqtt.Event
	recordName string
}

func New(opts Options) *Coordinator {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 256
	}
	if opts.RetryAttempts <= 0 {
		opts.RetryAttempts = 4
	}
	if opts.RetryBaseDelay <= 0 {
		opts.RetryBaseDelay = time.Second
	}
	if opts.SnapshotWait <= 0 {
		opts.SnapshotWait = 2 * time.Second
	}
	return &Coordinator{
		mqtt:           opts.MQTT,
		relay:          opts.Relay,
		snapshots:      opts.Snapshots,
		frigate:        opts.Frigate,
		logger:         logger.With("component", "bridge"),
		events:         make(chan mqtt.Event, opts.QueueSize),
		clips:          make(chan clipJob, 32),
		retryAttempts:  opts.RetryAttempts,
		retryBaseDelay: opts.RetryBaseDelay,
		snapshotWait:   opts.SnapshotWait,
		recorded:       map[string]string{},
	}
}

// Run blocks until ctx is done. It starts the workers, connects MQTT and
// forwards every decoded event to the relay (when relay != nil). Errors
// during forwarding are logged but don't terminate the loop.
func (c *Coordinator) Run(ctx context.Context) error {
	c.logger.Info("starting bridge",
		"dry_run", c.relay == nil,
		"snapshots_enabled", c.snapshots != nil || c.frigate != nil,
		"clips_enabled", c.frigate != nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.eventWorker(ctx) }()
	go func() { defer wg.Done(); c.clipWorker(ctx) }()

	if err := c.mqtt.Connect(ctx, c.Enqueue); err != nil {
		return err
	}
	defer c.mqtt.Disconnect()

	<-ctx.Done()
	wg.Wait()
	stats := c.Stats()
	c.logger.Info("shutting down",
		"received", stats.Received,
		"forwarded", stats.Forwarded,
		"skipped", stats.Skipped,
		"dropped", stats.Dropped,
		"errors", stats.Errors,
		"snapshot_uploads", stats.SnapshotUploads,
		"clip_uploads", stats.ClipUploads)
	return nil
}

type Stats struct {
	Received        int64 `json:"received"`
	Forwarded       int64 `json:"forwarded"`
	Skipped         int64 `json:"skipped"`
	Dropped         int64 `json:"dropped"`
	Errors          int64 `json:"errors"`
	SnapshotUploads int64 `json:"snapshot_uploads"`
	ClipUploads     int64 `json:"clip_uploads"`
}

func (c *Coordinator) Stats() Stats {
	return Stats{
		Received:        c.receivedCount.Load(),
		Forwarded:       c.forwardedCount.Load(),
		Skipped:         c.skippedCount.Load(),
		Dropped:         c.droppedCount.Load(),
		Errors:          c.errorCount.Load(),
		SnapshotUploads: c.snapshotUploads.Load(),
		ClipUploads:     c.clipUploads.Load(),
	}
}

// Enqueue is the MQTT handler. It never blocks: "update" messages are
// counted and dropped here, and if the queue is full (relay down for a
// long time) the event is dropped and counted rather than stalling the
// broker connection.
func (c *Coordinator) Enqueue(_ context.Context, ev mqtt.Event) {
	c.receivedCount.Add(1)
	switch ev.Phase {
	case mqtt.PhaseNew, mqtt.PhaseEnd:
	default:
		// "update" — Frigate sends these on score/zone changes; the
		// record already exists from "new".
		c.skippedCount.Add(1)
		return
	}
	select {
	case c.events <- ev:
	default:
		c.droppedCount.Add(1)
		c.logger.Warn("event queue full; dropping event", "id", ev.ID, "phase", ev.Phase)
	}
}

func (c *Coordinator) eventWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-c.events:
			switch ev.Phase {
			case mqtt.PhaseNew:
				c.handleNewEvent(ctx, ev)
			case mqtt.PhaseEnd:
				c.handleEndEvent(ctx, ev)
			}
		}
	}
}

func (c *Coordinator) clipWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-c.clips:
			c.backfillClip(ctx, job)
		}
	}
}

// retry runs op up to retryAttempts times with exponential backoff while
// the error is transient (network, 429, 5xx).
func (c *Coordinator) retry(ctx context.Context, what string, op func() error) error {
	delay := c.retryBaseDelay
	var err error
	for attempt := 1; attempt <= c.retryAttempts; attempt++ {
		if err = op(); err == nil || !relay.Retryable(err) || attempt == c.retryAttempts {
			return err
		}
		c.logger.Debug("transient relay error; retrying", "op", what, "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(delay):
		}
		delay *= 2
	}
	return err
}

func (c *Coordinator) handleNewEvent(ctx context.Context, ev mqtt.Event) {
	if c.relay == nil {
		c.skippedCount.Add(1)
		c.logger.Info("[dry-run] would forward",
			"id", ev.ID,
			"camera", ev.Camera,
			"label", ev.Label,
			"sub_label", ev.SubLabel,
			"score", ev.TopScore,
			"zones", ev.Zones)
		return
	}

	// Attach a snapshot when one belonging to THIS event is available.
	// A missing snapshot is never fatal: the alert ships without image.
	var snapshot *relay.AssetReceipt
	if jpeg := c.snapshotFor(ctx, ev); len(jpeg) > 0 {
		snapshot = c.uploadAsset(ctx, ev, relay.AssetFieldSnapshot, jpeg, "image/jpeg")
		if snapshot != nil {
			c.snapshotUploads.Add(1)
		}
	}

	zones := ev.Zones
	if zones == nil {
		zones = []string{}
	}
	var resp *relay.PostEventResponse
	err := c.retry(ctx, "post event", func() error {
		var err error
		resp, err = c.relay.PostEvent(ctx, relay.Event{
			ID:         ev.ID,
			Camera:     ev.Camera,
			Label:      ev.Label,
			DetectedAt: ev.StartTime.UnixMilli(),
			TopScore:   ev.TopScore,
			Zones:      zones,
		}, snapshot)
		return err
	})
	if err != nil {
		c.errorCount.Add(1)
		c.logger.Warn("forward to relay failed", "id", ev.ID, "err", err)
		return
	}
	c.forwardedCount.Add(1)
	recordName := resp.RecordName
	if recordName == "" {
		recordName = "frigate-" + ev.ID
	}
	c.mu.Lock()
	c.recorded[ev.ID] = recordName
	if len(c.recorded) > 4096 { // bound memory: events normally end within minutes
		for k := range c.recorded {
			delete(c.recorded, k)
			if len(c.recorded) <= 2048 {
				break
			}
		}
	}
	c.mu.Unlock()
	c.logger.Info("event forwarded",
		"id", ev.ID,
		"camera", ev.Camera,
		"label", ev.Label,
		"score", ev.TopScore,
		"zones", zones,
		"recordName", recordName,
		"snapshot", snapshot != nil)
}

// snapshotFor prefers Frigate's HTTP snapshot for this exact event id;
// otherwise it waits briefly for a live MQTT snapshot published since the
// event started. Retained snapshots replayed on connect are never used.
func (c *Coordinator) snapshotFor(ctx context.Context, ev mqtt.Event) []byte {
	if c.frigate != nil {
		jpeg, err := c.frigate.FetchSnapshot(ctx, ev.ID)
		if err == nil && len(jpeg) > 0 {
			return jpeg
		}
		if err != nil {
			c.logger.Debug("frigate snapshot fetch failed; trying MQTT cache", "id", ev.ID, "err", err)
		}
	}
	if c.snapshots == nil {
		return nil
	}
	since := ev.StartTime.Add(-2 * time.Second)
	deadline := time.Now().Add(c.snapshotWait)
	for {
		if jpeg := c.snapshots.Fresh(ev.Camera, ev.Label, since); len(jpeg) > 0 {
			return jpeg
		}
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// uploadAsset does the two-step CloudKit asset dance (request URL from
// the Worker, POST bytes to Apple's CDN). Returns nil on failure.
func (c *Coordinator) uploadAsset(ctx context.Context, ev mqtt.Event, field string, payload []byte, contentType string) *relay.AssetReceipt {
	var receipt *relay.AssetReceipt
	err := c.retry(ctx, "upload "+field, func() error {
		upload, err := c.relay.RequestAssetUpload(ctx, field, ev.ID)
		if err != nil {
			return err
		}
		receipt, err = c.relay.UploadAsset(ctx, upload.URL, payload, contentType)
		return err
	})
	if err != nil {
		c.logger.Warn("asset upload failed",
			"field", field, "id", ev.ID, "camera", ev.Camera, "bytes", len(payload), "err", err)
		return nil
	}
	return receipt
}

func (c *Coordinator) handleEndEvent(ctx context.Context, ev mqtt.Event) {
	// Clip backfill needs: Frigate HTTP configured, a recorded clip, a
	// relay client (not dry-run), and a record created by this bridge
	// for the event's "new" (otherwise there is nothing to attach to).
	if c.frigate == nil || !ev.HasClip || c.relay == nil {
		c.skippedCount.Add(1)
		return
	}
	c.mu.Lock()
	recordName, ok := c.recorded[ev.ID]
	delete(c.recorded, ev.ID)
	c.mu.Unlock()
	if !ok {
		c.skippedCount.Add(1)
		c.logger.Debug("end for an event this bridge did not record; skipping clip", "id", ev.ID)
		return
	}
	select {
	case c.clips <- clipJob{ev: ev, recordName: recordName}:
	default:
		c.droppedCount.Add(1)
		c.logger.Warn("clip queue full; skipping clip backfill", "id", ev.ID)
	}
}

func (c *Coordinator) backfillClip(ctx context.Context, job clipJob) {
	ev := job.ev
	mp4, err := c.frigate.FetchClip(ctx, ev.ID)
	if err != nil {
		c.errorCount.Add(1)
		c.logger.Warn("clip fetch from frigate failed", "id", ev.ID, "camera", ev.Camera, "err", err)
		return
	}
	if len(mp4) == 0 {
		c.skippedCount.Add(1)
		c.logger.Debug("clip not on disk; skipping backfill", "id", ev.ID, "camera", ev.Camera)
		return
	}
	receipt := c.uploadAsset(ctx, ev, relay.AssetFieldClip, mp4, "video/mp4")
	if receipt == nil {
		c.errorCount.Add(1)
		return
	}
	err = c.retry(ctx, "commit clip", func() error {
		return c.relay.UpdateEventAsset(ctx, job.recordName, relay.AssetFieldClip, receipt)
	})
	if err != nil {
		c.errorCount.Add(1)
		c.logger.Warn("clip commit to record failed", "id", ev.ID, "recordName", job.recordName, "err", err)
		return
	}
	c.clipUploads.Add(1)
	c.logger.Info("clip backfilled",
		"id", ev.ID, "camera", ev.Camera, "bytes", len(mp4), "recordName", job.recordName)
}
