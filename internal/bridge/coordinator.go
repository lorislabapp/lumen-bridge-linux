// Package bridge wires together the MQTT subscriber and the CloudKit
// writer. The Coordinator is the only object that owns a context cancel
// for the lifetime of the daemon — main() asks for one with Run() and
// closes it on SIGTERM/SIGINT.
package bridge

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/lorislabapp/lumen-bridge-linux/internal/cloudkit"
	"github.com/lorislabapp/lumen-bridge-linux/internal/mqtt"
)

type Options struct {
	MQTT     *mqtt.Client
	CK       *cloudkit.Client // optional — nil = dry-run, decode + log only
	Logger   *slog.Logger
}

type Coordinator struct {
	mqtt   *mqtt.Client
	ck     *cloudkit.Client
	logger *slog.Logger

	// Counters surfaced via /healthz and Run() shutdown logs. Atomic
	// because handleEvent runs on the paho receive goroutine and Snapshot
	// reads from the HTTP handler goroutine.
	receivedCount  atomic.Int64
	forwardedCount atomic.Int64
	skippedCount   atomic.Int64 // dry-run or non-"new" phases
	errorCount     atomic.Int64
}

func New(opts Options) *Coordinator {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Coordinator{
		mqtt:   opts.MQTT,
		ck:     opts.CK,
		logger: logger.With("component", "bridge"),
	}
}

// Run blocks until ctx is done. It connects MQTT, subscribes, and forwards
// every decoded event to CloudKit (when ck != nil). Errors during
// forwarding are logged but don't terminate the loop — the bridge is
// designed to keep running through transient broker / CloudKit hiccups.
func (c *Coordinator) Run(ctx context.Context) error {
	c.logger.Info("starting bridge", "dry_run", c.ck == nil)
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
		"errors", stats.Errors)
	return nil
}

// Stats is a point-in-time snapshot of the counters.
type Stats struct {
	Received  int64 `json:"received"`
	Forwarded int64 `json:"forwarded"`
	Skipped   int64 `json:"skipped"`
	Errors    int64 `json:"errors"`
}

func (c *Coordinator) Stats() Stats {
	return Stats{
		Received:  c.receivedCount.Load(),
		Forwarded: c.forwardedCount.Load(),
		Skipped:   c.skippedCount.Load(),
		Errors:    c.errorCount.Load(),
	}
}

func (c *Coordinator) handleEvent(ctx context.Context, ev mqtt.Event) {
	c.receivedCount.Add(1)

	if ev.Phase != mqtt.PhaseNew {
		// v0.0.1 only persists "new" events. Update / end phases will be
		// used in v0.4.0 for clip-MP4 backfill.
		c.skippedCount.Add(1)
		return
	}

	if c.ck == nil {
		// Dry-run: structured log only, no network call.
		c.skippedCount.Add(1)
		c.logger.Info("[dry-run] would forward",
			"id", ev.ID,
			"camera", ev.Camera,
			"label", ev.Label,
			"score", ev.TopScore,
			"zones", ev.Zones)
		return
	}

	rec := (&cloudkit.FrigateEvent{
		ID:        ev.ID,
		Camera:    ev.Camera,
		Label:     ev.Label,
		Zones:     ev.Zones,
		TopScore:  ev.TopScore,
		StartTime: ev.StartTime,
	}).ToRecord()

	if err := c.ck.SaveRecord(ctx, cloudkit.DBPrivate, rec); err != nil {
		c.errorCount.Add(1)
		c.logger.Warn("forward to CloudKit failed", "id", ev.ID, "err", err)
		return
	}
	c.forwardedCount.Add(1)
	c.logger.Info("event forwarded",
		"id", ev.ID,
		"camera", ev.Camera,
		"label", ev.Label,
		"score", ev.TopScore)
}
