package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/lorislabapp/lumen-bridge-linux/internal/bridge"
	"github.com/lorislabapp/lumen-bridge-linux/internal/config"
	"github.com/lorislabapp/lumen-bridge-linux/internal/frigate"
	"github.com/lorislabapp/lumen-bridge-linux/internal/health"
	"github.com/lorislabapp/lumen-bridge-linux/internal/mqtt"
	"github.com/lorislabapp/lumen-bridge-linux/internal/relay"
)

const version = "0.3.2"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "auth":
		// Deprecated in v0.3.0 — kept as an explicit error so users on
		// older docs get a clear pointer rather than silent breakage.
		fmt.Fprintln(os.Stderr, "`lumen-bridge auth` was removed in v0.3.0 (CloudKit Web Services sign-in is unreliable).")
		fmt.Fprintln(os.Stderr, "Use `lumen-bridge pair --code <6-digit>` instead, after generating a code in the iOS Lumen app.")
		os.Exit(2)
	case "pair":
		pairCmd(os.Args[2:])
	case "doctor":
		doctorCmd(os.Args[2:])
	case "version", "--version":
		fmt.Println("lumen-bridge", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand:", os.Args[1])
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `lumen-bridge — Frigate → Lumen notifications bridge (`+version+`)

Subcommands:
  run     start the bridge daemon (default for systemd / Docker)
            --config PATH       config.yaml location
            --dry-run           subscribe + decode events but skip relay writes
            --debug             verbose logging
            --health-addr ADDR  bind address for /healthz (default 127.0.0.1:9090)
  pair    pair with the Lumen app and store this bridge's device token
            --code CODE         6-digit code from Lumen → Paired Bridges (required)
            --config PATH       read relay.url / relay.device_token_path from here
            --token-path PATH   override where the token is written
            --relay URL         override the relay URL
  doctor  preflight check: config, MQTT reachability, device token,
          relay reachability, Frigate reachability. Exits non-zero on failure.
  version print version and exit

Configuration: ./config.yaml or /etc/lumen-bridge/config.yaml; every
field is overridable via env var (LB_*). See README.md.
`)
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to config.yaml")
	dryRun := fs.Bool("dry-run", false, "decode MQTT events without sending anything to the relay")
	debug := fs.Bool("debug", false, "verbose logging")
	healthAddr := fs.String("health-addr", defaultHealthAddr(), "bind address for /healthz; empty = disable")
	_ = fs.Parse(args)

	logger := newLogger(*debug)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("config load failed", "err", err)
		os.Exit(1)
	}

	logger.Info("config loaded",
		"version", version,
		"mqtt_host", cfg.MQTT.Host,
		"mqtt_port", cfg.MQTT.Port,
		"mqtt_tls", cfg.MQTT.TLS,
		"relay_url", cfg.Relay.URL,
		"frigate_base_url", cfg.Frigate.BaseURL,
		"dry_run", *dryRun,
		"health_addr", *healthAddr)

	snapshots := mqtt.NewSnapshotCache(30 * time.Minute)
	mqttCli := mqtt.New(mqtt.Options{
		Host:        cfg.MQTT.Host,
		Port:        cfg.MQTT.Port,
		Username:    cfg.MQTT.Username,
		Password:    cfg.MQTT.Password,
		TopicPrefix: cfg.MQTT.TopicPrefix,
		ClientID:    cfg.MQTT.ClientID,
		TLS:         cfg.MQTT.TLS,
		Snapshots:   snapshots,
		Logger:      logger,
	})

	var relayCli *relay.Client
	if !*dryRun {
		stored, err := relay.LoadDeviceToken(cfg.Relay.DeviceTokenPath)
		if err != nil {
			logger.Error("device token load failed", "err", err, "path", cfg.Relay.DeviceTokenPath)
			os.Exit(1)
		}
		if stored == nil {
			logger.Error("no device token — run `lumen-bridge pair --code <6-digit>` after generating a code in the Lumen iOS app, or set LB_RELAY_DEVICE_TOKEN, or pass --dry-run")
			os.Exit(1)
		}
		relayCli, err = relay.New(relay.Options{
			RelayURL:    cfg.Relay.URL,
			DeviceToken: stored.Token,
			Logger:      logger,
		})
		if err != nil {
			logger.Error("relay client init failed", "err", err)
			os.Exit(1)
		}
	}

	// Interfaces must stay nil (not a typed nil pointer) when a component
	// is disabled, or the coordinator would think it is configured.
	opts := bridge.Options{
		MQTT:      mqttCli,
		Snapshots: snapshots,
		Logger:    logger,
	}
	if relayCli != nil {
		opts.Relay = relayCli
	}
	if cfg.Frigate.BaseURL != "" {
		opts.Frigate = frigate.New(cfg.Frigate.BaseURL)
	}
	coord := bridge.New(opts)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Periodic snapshot cache sweep — drops entries older than the cache
	// TTL so a long-lived daemon doesn't accumulate stale JPEGs for
	// cameras that have been removed from Frigate's config.
	go sweepSnapshots(ctx, snapshots, logger)

	var wg sync.WaitGroup
	if *healthAddr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := health.New(*healthAddr, func() any { return coord.Stats() }, mqttCli, logger)
			if err := h.Serve(ctx); err != nil {
				logger.Error("health server exited", "err", err)
			}
		}()
	}

	if err := coord.Run(ctx); err != nil {
		logger.Error("coordinator exited with error", "err", err)
		cancel()
		wg.Wait()
		os.Exit(1)
	}
	wg.Wait()
}

// authCmd removed in v0.3.0 — see the switch on os.Args[1] for the
// deprecation message users see. The dispatcher exits before reaching
// any handler.

// sweepSnapshots runs Sweep() every 5 minutes to drop expired retained
// JPEGs. Lightweight; just walks the cache map under a write lock.
func sweepSnapshots(ctx context.Context, cache *mqtt.SnapshotCache, logger *slog.Logger) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if dropped := cache.Sweep(); dropped > 0 {
				logger.Debug("snapshot cache swept", "dropped", dropped)
			}
		}
	}
}

func defaultHealthAddr() string {
	if v := os.Getenv("LB_HEALTH_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:9090"
}

func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
