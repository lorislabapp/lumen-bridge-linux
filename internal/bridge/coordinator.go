// Package bridge wires together the MQTT subscriber and the relay HTTP
// client that forwards Frigate events to the Lumen Bridge Relay Worker.
// The Coordinator owns the receive/forward counters and the per-event
// pipeline.
//
// Pre-2026-05-21 the Coordinator wrote directly to CloudKit via a
// container-API-token + ckSession flow. That path is dead — Apple's
// IDMSA web sign-in is unreliable for end-users — and the daemon now
// posts to the Worker, which signs Server-to-Server requests on the
// bridge's behalf using a per-user device token from the pair flow.
// See memory/project_relay_proxy_migration.md.
package bridge

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/lorislabapp/lumen-bridge-linux/internal/frigate"
	"github.com/lorislabapp/lumen-bridge-linux/internal/mqtt"
	"github.com/lorislabapp/lumen-bridge-linux/internal/relay"
)

type Options struct {
	MQTT      *mqtt.Client
	Relay     *relay.Client       // required in prod — nil = dry-run, decode + log only
	Snapshots *mqtt.SnapshotCache // optional — Phase 2.5 will use this once asset upload via the Worker is wired
	Frigate   *frigate.Client     // optional — Phase 2.5 clip backfill
	Logger    *slog.Logger
}

type Coordinator struct {
	mqtt      *mqtt.Client
	relay     *relay.Client
	snapshots *mqtt.SnapshotCache
	frigate   *frigate.Client
	logger    *slog.Logger

	receivedCount   atomic.Int64
	forwardedCount  atomic.Int64
	skippedCount    atomic.Int64
	errorCount      atomic.Int64
	snapshotUploads atomic.Int64
	clipUploads     atomic.Int64
}

func New(opts Options) *Coordinator {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Coordinator{
		mqtt:      opts.MQTT,
		relay:     opts.Relay,
		snapshots: opts.Snapshots,
		frigate:   opts.Frigate,
		logger:    logger.With("component", "bridge"),
	}
}

// Run blocks until ctx is done. It connects MQTT, subscribes, and forwards
// every decoded event to the relay (when relay != nil). Errors during
// forwarding are logged but don't terminate the loop — the bridge is
// designed to keep running through transient broker / relay hiccups.
func (c *Coordinator) Run(ctx context.Context) error {
	c.logger.Info("starting bridge",
		"dry_run", c.relay == nil,
		"snapshots_enabled", c.snapshots != nil,
		"clips_enabled", c.frigate != nil)
	if err := c.mqtt.Connect(ctx, c.handleEvent); err != nil {
		return err
	}
	defer c.mqtt.Disconnect()

	<-ctx.Done()
	stats := c.Stats()
	c.logger.Info("shutting down",
		"received", stats.Received,
		"forwarded", stats.Forwarded,
		"skipped", stats.Skipped,
		"errors", stats.Errors,
		"snapshot_uploads", stats.SnapshotUploads,
		"clip_uploads", stats.ClipUploads)
	return nil
}

type Stats struct {
	Received        int64 `json:"received"`
	Forwarded       int64 `json:"forwarded"`
	Skipped         int64 `json:"skipped"`
	Errors          int64 `json:"errors"`
	SnapshotUploads int64 `json:"snapshot_uploads"`
	ClipUploads     int64 `json:"clip_uploads"`
}

func (c *Coordinator) Stats() Stats {
	return Stats{
		Received:        c.receivedCount.Load(),
		Forwarded:       c.forwardedCount.Load(),
		Skipped:         c.skippedCount.Load(),
		Errors:          c.errorCount.Load(),
		SnapshotUploads: c.snapshotUploads.Load(),
		ClipUploads:     c.clipUploads.Load(),
	}
}

func (c *Coordinator) handleEvent(ctx context.Context, ev mqtt.Event) {
	c.receivedCount.Add(1)

	switch ev.Phase {
	case mqtt.PhaseNew:
		c.handleNewEvent(ctx, ev)
	case mqtt.PhaseEnd:
		c.handleEndEvent(ctx, ev)
	default:
		// "update" phase — Frigate sends these on score/zone changes; we
		// don't write anything because the iOS clients already have the
		// "new" record. Counted as skipped so /metrics shows the volume.
		c.skippedCount.Add(1)
	}
}

func (c *Coordinator) handleNewEvent(ctx context.Context, ev mqtt.Event) {
	if c.relay == nil {
		c.skippedCount.Add(1)
		c.logger.Info("[dry-run] would forward",
			"id", ev.ID,
			"camera", ev.Camera,
			"label", ev.Label,
			"score", ev.TopScore,
			"zones", ev.Zones)
		return
	}

	// Phase 2.5: try to attach a snapshot. The snapshot cache is keyed
	// on (camera, label) and is populated by retained MQTT messages on
	// frigate/{camera}/{label}/snapshot. If no snapshot is cached yet
	// (cold start, label mismatch), we ship the record metadata-only
	// rather than block the event entirely — iOS still gets the
	// notification, just without the thumbnail.
	var snapshot *relay.AssetReceipt
	if c.snapshots != nil {
		if jpeg := c.snapshots.Latest(ev.Camera, ev.Label); len(jpeg) > 0 {
			snapshot = c.uploadSnapshot(ctx, ev, jpeg)
		}
	}

	resp, err := c.relay.PostEvent(ctx, relay.Event{
		ID:         ev.ID,
		Camera:     ev.Camera,
		Label:      ev.Label,
		DetectedAt: ev.StartTime.UnixMilli(),
		TopScore:   ev.TopScore,
		Zones:      ev.Zones,
	}, snapshot)
	if err != nil {
		c.errorCount.Add(1)
		c.logger.Warn("forward to relay failed", "id", ev.ID, "err", err)
		return
	}
	c.forwardedCount.Add(1)
	c.logger.Info("event forwarded",
		"id", ev.ID,
		"camera", ev.Camera,
		"label", ev.Label,
		"score", ev.TopScore,
		"recordName", resp.RecordName,
		"snapshot", snapshot != nil)
}

// uploadSnapshot does the two-step CloudKit asset dance (request URL
// from Worker, PUT bytes to Apple CDN). Returns nil on any failure
// because a missing snapshot is non-fatal — the event still ships
// metadata-only.
func (c *Coordinator) uploadSnapshot(ctx context.Context, ev mqtt.Event, jpeg []byte) *relay.AssetReceipt {
	upload, err := c.relay.RequestAssetUpload(ctx, relay.AssetFieldSnapshot, ev.ID)
	if err != nil {
		c.logger.Warn("snapshot upload-url request failed",
			"id", ev.ID, "camera", ev.Camera, "err", err)
		return nil
	}
	receipt, err := c.relay.UploadAsset(ctx, upload.URL, jpeg, "image/jpeg")
	if err != nil {
		c.logger.Warn("snapshot PUT to cloudkit failed",
			"id", ev.ID, "camera", ev.Camera, "bytes", len(jpeg), "err", err)
		return nil
	}
	c.snapshotUploads.Add(1)
	c.logger.Debug("snapshot uploaded",
		"id", ev.ID, "camera", ev.Camera, "bytes", len(jpeg),
		"receipt", truncateString(receipt.Receipt, 20))
	return receipt
}

func (c *Coordinator) handleEndEvent(ctx context.Context, ev mqtt.Event) {
	// Frigate publishes "end" once the event is finalised and (when
	// configured) the MP4 clip is written to disk. We try to fetch the
	// clip from Frigate's HTTP API and backfill it onto the existing
	// FrigateEvent record so the iOS clients can play it back in-app.
	//
	// Three preconditions: Frigate client configured, clip recorded,
	// and the Bridge has a relay client (not dry-run).
	if c.frigate == nil || !ev.HasClip || c.relay == nil {
		c.skippedCount.Add(1)
		return
	}

	mp4, err := c.frigate.FetchClip(ctx, ev.ID)
	if err != nil {
		c.errorCount.Add(1)
		c.logger.Warn("clip fetch from frigate failed",
			"id", ev.ID, "camera", ev.Camera, "err", err)
		return
	}
	if len(mp4) == 0 {
		// (nil, nil) from FetchClip = "Frigate skipped the recording".
		// Not an error — just nothing to backfill.
		c.skippedCount.Add(1)
		c.logger.Debug("clip not on disk; skipping backfill",
			"id", ev.ID, "camera", ev.Camera)
		return
	}

	upload, err := c.relay.RequestAssetUpload(ctx, relay.AssetFieldClip, ev.ID)
	if err != nil {
		c.errorCount.Add(1)
		c.logger.Warn("clip upload-url request failed",
			"id", ev.ID, "camera", ev.Camera, "err", err)
		return
	}
	receipt, err := c.relay.UploadAsset(ctx, upload.URL, mp4, "video/mp4")
	if err != nil {
		c.errorCount.Add(1)
		c.logger.Warn("clip PUT to cloudkit failed",
			"id", ev.ID, "camera", ev.Camera, "bytes", len(mp4), "err", err)
		return
	}
	if err := c.relay.UpdateEventAsset(ctx, upload.RecordName, relay.AssetFieldClip, receipt); err != nil {
		c.errorCount.Add(1)
		c.logger.Warn("clip commit to record failed",
			"id", ev.ID, "recordName", upload.RecordName, "err", err)
		return
	}
	c.clipUploads.Add(1)
	c.logger.Info("clip backfilled",
		"id", ev.ID, "camera", ev.Camera, "bytes", len(mp4),
		"recordName", upload.RecordName)
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
