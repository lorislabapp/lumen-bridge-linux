// Package bridge wires together the MQTT subscriber and the CloudKit
// writer. The Coordinator is the only object that owns a context cancel
// for the lifetime of the daemon — main() asks for one with Run() and
// closes it on SIGTERM/SIGINT.
package bridge

import (
	"context"
	"log/slog"

	"github.com/lorislabapp/lumen-bridge-linux/internal/cloudkit"
	"github.com/lorislabapp/lumen-bridge-linux/internal/mqtt"
)

type Coordinator struct {
	mqtt     *mqtt.Client
	ck       *cloudkit.Client
	logger   *slog.Logger

	// receivedCount + forwardedCount are atomic counters surfaced for
	// observability via the future /metrics endpoint (v0.5.0).
	receivedCount  int
	forwardedCount int
}

func New(m *mqtt.Client, ck *cloudkit.Client, logger *slog.Logger) *Coordinator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Coordinator{
		mqtt:   m,
		ck:     ck,
		logger: logger.With("component", "bridge"),
	}
}

// Run blocks until ctx is done. It connects MQTT, subscribes, and forwards
// every decoded event to CloudKit. Errors during forwarding are logged but
// don't terminate the loop — the bridge is designed to keep running even
// when individual events fail (e.g. brief CloudKit outage).
func (c *Coordinator) Run(ctx context.Context) error {
	c.logger.Info("starting bridge")
	if err := c.mqtt.Connect(ctx, c.handleEvent); err != nil {
		return err
	}
	defer c.mqtt.Disconnect()

	<-ctx.Done()
	c.logger.Info("shutting down", "received", c.receivedCount, "forwarded", c.forwardedCount)
	return nil
}

func (c *Coordinator) handleEvent(ctx context.Context, ev mqtt.Event) {
	c.receivedCount++
	if ev.Phase != mqtt.PhaseNew {
		// v0.0.1 only persists "new" events. Update / end phases will be
		// used in v0.4.0 for clip-MP4 backfill.
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
		c.logger.Warn("forward to CloudKit failed", "id", ev.ID, "err", err)
		return
	}
	c.forwardedCount++
	c.logger.Info("event forwarded",
		"id", ev.ID,
		"camera", ev.Camera,
		"label", ev.Label,
		"score", ev.TopScore)
}
