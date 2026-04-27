// Package bridge wires together the MQTT subscriber, the snapshot cache,
// the Frigate HTTP client, and the CloudKit writer. The Coordinator owns
// the receive-count / forward-count counters and the per-event upload
// pipeline (snapshot first, record write second, clip backfill on `end`).
package bridge

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/lorislabapp/lumen-bridge-linux/internal/cloudkit"
	"github.com/lorislabapp/lumen-bridge-linux/internal/frigate"
	"github.com/lorislabapp/lumen-bridge-linux/internal/mqtt"
)

type Options struct {
	MQTT      *mqtt.Client
	CK        *cloudkit.Client     // optional — nil = dry-run, decode + log only
	Snapshots *mqtt.SnapshotCache  // optional — nil = no snapshot upload
	Frigate   *frigate.Client      // optional — nil = no clip backfill
	Logger    *slog.Logger
}

type Coordinator struct {
	mqtt      *mqtt.Client
	ck        *cloudkit.Client
	snapshots *mqtt.SnapshotCache
	frigate   *frigate.Client
	logger    *slog.Logger

	receivedCount  atomic.Int64
	forwardedCount atomic.Int64
	skippedCount   atomic.Int64
	errorCount     atomic.Int64
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
		ck:        opts.CK,
		snapshots: opts.Snapshots,
		frigate:   opts.Frigate,
		logger:    logger.With("component", "bridge"),
	}
}

// Run blocks until ctx is done. It connects MQTT, subscribes, and forwards
// every decoded event to CloudKit (when ck != nil). Errors during
// forwarding are logged but don't terminate the loop — the bridge is
// designed to keep running through transient broker / CloudKit hiccups.
func (c *Coordinator) Run(ctx context.Context) error {
	c.logger.Info("starting bridge",
		"dry_run", c.ck == nil,
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
	if c.ck == nil {
		c.skippedCount.Add(1)
		c.logger.Info("[dry-run] would forward",
			"id", ev.ID,
			"camera", ev.Camera,
			"label", ev.Label,
			"score", ev.TopScore,
			"zones", ev.Zones)
		return
	}

	rec := cloudkit.FrigateEvent{
		ID:        ev.ID,
		Camera:    ev.Camera,
		Label:     ev.Label,
		Zones:     ev.Zones,
		TopScore:  ev.TopScore,
		StartTime: ev.StartTime,
	}

	// Snapshot upload first — if the upload fails, log and continue
	// without it (the record is more important than the preview image).
	if c.snapshots != nil && ev.HasSnapshot {
		if jpeg := c.snapshots.Latest(ev.Camera, ev.Label); len(jpeg) > 0 {
			receipt, err := c.ck.UploadAsset(ctx, cloudkit.DBPrivate,
				"FrigateEvent", ev.ID, "snapshot", jpeg)
			if err != nil {
				c.logger.Warn("snapshot upload failed",
					"id", ev.ID, "size", len(jpeg), "err", err)
			} else {
				rec.Snapshot = receipt
				c.snapshotUploads.Add(1)
			}
		}
	}

	if err := c.ck.SaveRecord(ctx, cloudkit.DBPrivate, rec.ToRecord()); err != nil {
		c.errorCount.Add(1)
		c.logger.Warn("forward to CloudKit failed", "id", ev.ID, "err", err)
		return
	}
	c.forwardedCount.Add(1)
	c.logger.Info("event forwarded",
		"id", ev.ID,
		"camera", ev.Camera,
		"label", ev.Label,
		"score", ev.TopScore,
		"snapshot", rec.Snapshot != nil)
}

func (c *Coordinator) handleEndEvent(ctx context.Context, ev mqtt.Event) {
	// Clip backfill: only if Frigate said it kept a clip AND we have an
	// HTTP client to fetch it AND we have CK to upload to.
	if c.frigate == nil || c.ck == nil || !ev.HasClip {
		c.skippedCount.Add(1)
		return
	}
	clip, err := c.frigate.FetchClip(ctx, ev.ID)
	if err != nil {
		c.errorCount.Add(1)
		c.logger.Warn("fetch clip failed", "id", ev.ID, "err", err)
		return
	}
	if len(clip) == 0 {
		// 404 from Frigate — event existed but no MP4 on disk. Counted
		// as skipped so we still see the event volume in metrics.
		c.skippedCount.Add(1)
		return
	}
	receipt, err := c.ck.UploadAsset(ctx, cloudkit.DBPrivate,
		"FrigateEvent", ev.ID, "clip", clip)
	if err != nil {
		c.errorCount.Add(1)
		c.logger.Warn("clip upload failed",
			"id", ev.ID, "size", len(clip), "err", err)
		return
	}
	// Patch the existing record with the new clip field. We don't have a
	// dedicated PATCH path yet — re-saving the record with the clip
	// attached relies on CloudKit's "forceUpdate" operation which merges.
	rec := cloudkit.FrigateEvent{
		ID:        ev.ID,
		Camera:    ev.Camera,
		Label:     ev.Label,
		Zones:     ev.Zones,
		TopScore:  ev.TopScore,
		StartTime: ev.StartTime,
		Clip:      receipt,
	}
	if err := c.ck.SaveRecord(ctx, cloudkit.DBPrivate, rec.ToRecord()); err != nil {
		c.errorCount.Add(1)
		c.logger.Warn("clip-attach SaveRecord failed", "id", ev.ID, "err", err)
		return
	}
	c.clipUploads.Add(1)
	c.logger.Info("clip attached",
		"id", ev.ID, "size", len(clip), "camera", ev.Camera)
}
