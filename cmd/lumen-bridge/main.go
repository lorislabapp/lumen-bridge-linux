package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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

const version = "0.3.0"

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
	fmt.Fprint(os.Stderr, `lumen-bridge — Frigate ↔ Lumen Bridge Relay (`+version+`)

Subcommands:
  run     start the bridge daemon (default for systemd / Docker)
            --config PATH       config.yaml location
            --dry-run           subscribe + decode events but skip relay writes
            --debug             verbose logging
            --health-addr ADDR  bind address for /healthz (default 127.0.0.1:9090)
  pair    pair with Lumen app to receive a device token from the relay
            --code CODE         6-digit pairing code from app (required)
            --relay URL         relay server URL (default wss://relay.lorislab.fr)
  doctor  preflight check: config, MQTT reachability, Frigate reachability,
          relay reachability. Exits non-zero on any check failure.
  version print version and exit

Configuration: ./config.yaml or /etc/lumen-bridge/config.yaml; every
field is overridable via env var (LB_*). See README.md.

NOTE on v0.3.0: the CloudKit Web Services sign-in flow (`+"`lumen-bridge auth`"+`)
was removed because Apple's IDMSA web auth is unreliable for end-users.
The daemon now talks to the Lumen Bridge Relay Worker via a per-user
device token from the pair flow. See the project memory note
project_relay_proxy_migration.md.
`)
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to config.yaml")
	dryRun := fs.Bool("dry-run", false, "decode MQTT events without writing to CloudKit")
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

	var frigateCli *frigate.Client
	if cfg.Frigate.BaseURL != "" {
		frigateCli = frigate.New(cfg.Frigate.BaseURL)
	}

	coord := bridge.New(bridge.Options{
		MQTT:      mqttCli,
		Relay:     relayCli,
		Snapshots: snapshots,
		Frigate:   frigateCli,
		Logger:    logger,
	})

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

// doctorCmd runs a preflight check before the user starts the daemon
// for real. Each step is independent so a single failure (e.g. Frigate
// HTTP unreachable) doesn't mask the rest.
func doctorCmd(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to config.yaml")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	check("config load", err)

	// MQTT TCP reachability — much cheaper than a full CONNECT, lets us
	// distinguish "DNS broken" from "broker rejected auth".
	dialAddr := net.JoinHostPort(cfg.MQTT.Host, fmt.Sprintf("%d", cfg.MQTT.Port))
	conn, err := net.DialTimeout("tcp", dialAddr, 5*time.Second)
	if err == nil {
		_ = conn.Close()
	}
	check("mqtt tcp reach "+dialAddr, err)

	// Device token — present?
	stored, err := relay.LoadDeviceToken(cfg.Relay.DeviceTokenPath)
	check("device token load", err)
	if stored != nil {
		fmt.Println("  device token: present")
		if stored.UserRef != "" {
			fmt.Println("  user ref:    ", stored.UserRef)
		}
	} else {
		fmt.Println("  ⚠ device token: missing — daemon will only run with --dry-run; run `lumen-bridge pair --code <6-digit>` after generating a code in the Lumen iOS app")
	}

	// Relay reachability — best-effort GET /health, doesn't fail
	// preflight if down because the daemon retries forever anyway.
	relayHealth := cfg.Relay.URL + "/health"
	if req, err := http.NewRequest(http.MethodGet, relayHealth, nil); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req = req.WithContext(ctx)
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		check("relay http "+relayHealth, err)
	}

	// Frigate HTTP — only checked if base_url is configured.
	if cfg.Frigate.BaseURL != "" {
		req, _ := http.NewRequest(http.MethodGet, cfg.Frigate.BaseURL+"/api/version", nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req = req.WithContext(ctx)
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		check("frigate http "+cfg.Frigate.BaseURL, err)
	} else {
		fmt.Println("  frigate http: skipped (frigate.base_url not set — clip backfill disabled)")
	}

	fmt.Println()
	fmt.Println("✓ doctor complete")
}

func check(label string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", label, err)
		os.Exit(1)
	}
	fmt.Printf("  ✓ %s\n", label)
}

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
