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

	"github.com/lorislabapp/lumen-bridge-linux/internal/auth"
	"github.com/lorislabapp/lumen-bridge-linux/internal/bridge"
	"github.com/lorislabapp/lumen-bridge-linux/internal/cloudkit"
	"github.com/lorislabapp/lumen-bridge-linux/internal/config"
	"github.com/lorislabapp/lumen-bridge-linux/internal/health"
	"github.com/lorislabapp/lumen-bridge-linux/internal/mqtt"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "auth":
		authCmd(os.Args[2:])
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
	fmt.Fprint(os.Stderr, `lumen-bridge — Frigate ↔ iCloud CloudKit relay (`+version+`)

Subcommands:
  run     start the bridge daemon (default for systemd / Docker)
            --config PATH       config.yaml location
            --dry-run           subscribe + decode events but skip CloudKit writes
            --debug             verbose logging
            --health-addr ADDR  bind address for /healthz (default 127.0.0.1:9090)
  auth    interactive Apple ID sign-in (one-time per host)
            --bind-addr ADDR    bind address for the local paste form (default 127.0.0.1:0)
  version print version and exit

Configuration: ./config.yaml or /etc/lumen-bridge/config.yaml; every
field is overridable via env var (LB_*). See README.md and docs/AUTH.md.
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
		"ck_container", cfg.CloudKit.Container,
		"ck_environment", cfg.CloudKit.Environment,
		"dry_run", *dryRun,
		"health_addr", *healthAddr)

	mqttCli := mqtt.New(mqtt.Options{
		Host:        cfg.MQTT.Host,
		Port:        cfg.MQTT.Port,
		Username:    cfg.MQTT.Username,
		Password:    cfg.MQTT.Password,
		TopicPrefix: cfg.MQTT.TopicPrefix,
		ClientID:    cfg.MQTT.ClientID,
		Logger:      logger,
	})

	var ckCli *cloudkit.Client
	if !*dryRun {
		tokens, err := auth.Load(cfg.CloudKit.UserTokenPath)
		if err != nil {
			logger.Error("auth load failed", "err", err)
			os.Exit(1)
		}
		if tokens == nil {
			logger.Error("no tokens; run `lumen-bridge auth` or set LB_CK_API_TOKEN + LB_CK_USER_TOKEN, or pass --dry-run to test without CloudKit")
			os.Exit(1)
		}
		ckCli = cloudkit.New(cloudkit.Options{
			Container:   cfg.CloudKit.Container,
			Environment: cloudkit.Environment(cfg.CloudKit.Environment),
			APIToken:    tokens.APIToken,
			UserToken:   tokens.UserToken,
			Logger:      logger,
		})
	}

	coord := bridge.New(bridge.Options{
		MQTT:   mqttCli,
		CK:     ckCli,
		Logger: logger,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

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

func authCmd(args []string) {
	fs := flag.NewFlagSet("auth", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to config.yaml")
	bindAddr := fs.String("bind-addr", "127.0.0.1:0", "where to listen for the paste form")
	debug := fs.Bool("debug", false, "verbose logging")
	_ = fs.Parse(args)

	logger := newLogger(*debug)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("config load failed", "err", err)
		os.Exit(1)
	}

	apiToken := os.Getenv("LB_CK_API_TOKEN")
	if apiToken == "" {
		logger.Error("LB_CK_API_TOKEN env var is required for auth (this is the *container* API token; generate one in CloudKit Dashboard — see docs/AUTH.md)")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	tokens, err := auth.Interactive(ctx, auth.InteractiveOptions{
		APIToken:   apiToken,
		OutputPath: cfg.CloudKit.UserTokenPath,
		BindAddr:   *bindAddr,
		Timeout:    10 * time.Minute,
		NotifyReady: func(localURL, helperURL string) {
			fmt.Println()
			fmt.Println("✦ Lumen Bridge — sign-in helper")
			fmt.Println()
			fmt.Println("  1. Open this URL in any browser to walk through sign-in:")
			fmt.Println("       ", localURL)
			fmt.Println()
			fmt.Println("  2. The form will direct you to Apple's sign-in page; on success")
			fmt.Println("     paste the resulting ckSession token back into the form.")
			fmt.Println()
			fmt.Println("  Token will be saved to:", cfg.CloudKit.UserTokenPath)
			fmt.Println("  Listening for the form submission… (timeout: 10 min)")
			fmt.Println()
		},
	})
	if err != nil {
		logger.Error("auth failed", "err", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("✓ token saved to", cfg.CloudKit.UserTokenPath)
	fmt.Println("  issued at:", tokens.IssuedAt.Format(time.RFC3339))
	fmt.Println()
	fmt.Println("  Next:")
	fmt.Println("    lumen-bridge run")
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
