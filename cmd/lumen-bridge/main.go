package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/lorislabapp/lumen-bridge-linux/internal/auth"
	"github.com/lorislabapp/lumen-bridge-linux/internal/bridge"
	"github.com/lorislabapp/lumen-bridge-linux/internal/cloudkit"
	"github.com/lorislabapp/lumen-bridge-linux/internal/config"
	"github.com/lorislabapp/lumen-bridge-linux/internal/mqtt"
)

const version = "0.0.1"

func main() {
	root := flag.NewFlagSet("lumen-bridge", flag.ExitOnError)
	root.Usage = printUsage

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
  auth    interactive Apple ID sign-in (one-time per host)
  version print version and exit

Configuration:
  Reads ./config.yaml (or /etc/lumen-bridge/config.yaml).
  Every field is overridable via env var (LB_*). See README.md.

Authentication:
  v0.0.1 expects LB_CK_API_TOKEN and LB_CK_USER_TOKEN env vars (the web
  flow lands in v0.2.0). See docs/AUTH.md.
`)
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to config.yaml (default: ./config.yaml or /etc/lumen-bridge/config.yaml)")
	dryRun := fs.Bool("dry-run", false, "decode MQTT events but skip CloudKit writes (useful for testing)")
	debug := fs.Bool("debug", false, "verbose logging")
	_ = fs.Parse(args)

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("config load failed", "err", err)
		os.Exit(1)
	}
	logger.Info("config loaded",
		"mqtt_host", cfg.MQTT.Host,
		"mqtt_port", cfg.MQTT.Port,
		"ck_container", cfg.CloudKit.Container,
		"ck_environment", cfg.CloudKit.Environment,
		"dry_run", *dryRun)

	tokens, err := auth.Load(cfg.CloudKit.UserTokenPath)
	if err != nil {
		logger.Error("auth load failed", "err", err)
		os.Exit(1)
	}
	if tokens == nil {
		logger.Error("no tokens — run `lumen-bridge auth` first or set LB_CK_API_TOKEN + LB_CK_USER_TOKEN")
		os.Exit(1)
	}

	mqttCli := mqtt.New(mqtt.Options{
		Host:        cfg.MQTT.Host,
		Port:        cfg.MQTT.Port,
		Username:    cfg.MQTT.Username,
		Password:    cfg.MQTT.Password,
		TopicPrefix: cfg.MQTT.TopicPrefix,
		ClientID:    cfg.MQTT.ClientID,
		Logger:      logger,
	})
	ckCli := cloudkit.New(cloudkit.Options{
		Container:   cfg.CloudKit.Container,
		Environment: cloudkit.Environment(cfg.CloudKit.Environment),
		APIToken:    tokens.APIToken,
		UserToken:   tokens.UserToken,
		Logger:      logger,
	})

	coord := bridge.New(mqttCli, ckCli, logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := coord.Run(ctx); err != nil {
		logger.Error("coordinator exited with error", "err", err)
		os.Exit(1)
	}
}

func authCmd(_ []string) {
	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "config load failed:", err)
		os.Exit(1)
	}
	if _, err := auth.Interactive(cfg.CloudKit.UserTokenPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
