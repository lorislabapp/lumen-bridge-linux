package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lorislabapp/lumen-bridge-linux/internal/config"
	"github.com/lorislabapp/lumen-bridge-linux/internal/relay"
)

// systemdConfigPath is the file the systemd unit from install.sh reads.
const systemdConfigPath = "/etc/lumen-bridge/config.yaml"

// installerExampleHost is the placeholder written by install.sh and
// config.example.yaml. Seeing it means the user edited a different file.
const installerExampleHost = "192.168.1.50"

const doctorTimeout = 5 * time.Second

// doctorCmd runs a preflight check before the user starts the daemon for
// real. Every check runs even if an earlier one fails, so one problem does
// not hide the others. Exits 1 if any check failed.
func doctorCmd(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to config.yaml")
	_ = fs.Parse(args)

	d := &doctor{out: os.Stdout, inContainer: inContainer()}
	os.Exit(d.run(*cfgPath))
}

type doctor struct {
	out         io.Writer
	inContainer bool
	failures    int
	warnings    int

	// Test hooks; nil means the real network.
	lookupHost func(ctx context.Context, host string) ([]string, error)
	dial       func(ctx context.Context, network, addr string) (net.Conn, error)
	httpClient *http.Client
}

func (d *doctor) ok(format string, a ...any)   { fmt.Fprintf(d.out, "  ✓ "+format+"\n", a...) }
func (d *doctor) info(format string, a ...any) { fmt.Fprintf(d.out, "    "+format+"\n", a...) }

func (d *doctor) warn(format string, a ...any) {
	d.warnings++
	fmt.Fprintf(d.out, "  ⚠ "+format+"\n", a...)
}

func (d *doctor) fail(label string, err error, hints ...string) {
	d.failures++
	fmt.Fprintf(d.out, "  ✗ %s: %v\n", label, err)
	for _, h := range hints {
		fmt.Fprintf(d.out, "    → %s\n", h)
	}
}

func (d *doctor) run(cfgPath string) int {
	fmt.Fprintf(d.out, "lumen-bridge %s doctor\n\n", version)

	cfg, src, err := config.LoadWithSource(cfgPath)
	d.reportSource(cfgPath, src)
	if err != nil {
		d.fail("config load", err, configHints(err, src)...)
		return d.finish()
	}
	d.ok("config load")

	d.checkMQTT(cfg, src)
	d.checkDeviceToken(cfg)
	d.checkRelay(cfg)
	d.checkFrigate(cfg)
	return d.finish()
}

func (d *doctor) finish() int {
	fmt.Fprintln(d.out)
	if d.failures > 0 {
		fmt.Fprintf(d.out, "✗ doctor found %d problem(s)\n", d.failures)
		return 1
	}
	if d.warnings > 0 {
		fmt.Fprintf(d.out, "✓ doctor complete (%d warning(s))\n", d.warnings)
		return 0
	}
	fmt.Fprintln(d.out, "✓ doctor complete")
	return 0
}

// reportSource prints where the config came from. Most "doctor uses an
// address I don't have" reports come from reading a different file than
// the one the user edited.
func (d *doctor) reportSource(cfgPath string, src config.Source) {
	switch {
	case src.File != "" && cfgPath != "":
		d.info("config file: %s (from --config)", src.File)
	case src.File != "":
		d.info("config file: %s (found automatically; pass --config to choose)", src.File)
	default:
		d.info("config file: none found (searched %s)", strings.Join(config.SearchPaths, ", "))
	}
	if len(src.Env) > 0 {
		d.info("env overrides: %s", strings.Join(src.Env, ", "))
	}
	if d.inContainer {
		d.info("running inside a container")
	}
	if src.File != "" && src.File != systemdConfigPath && !d.inContainer {
		if _, err := os.Stat(systemdConfigPath); err == nil {
			d.info("note: the systemd service reads %s, not this file", systemdConfigPath)
		}
	}
	fmt.Fprintln(d.out)
}

func configHints(err error, src config.Source) []string {
	var hints []string
	if errors.Is(err, os.ErrPermission) {
		hints = append(hints, "run as the service user: sudo -u lumen-bridge lumen-bridge doctor --config "+src.File)
	}
	if errors.Is(err, os.ErrNotExist) {
		hints = append(hints, "check the --config path; inside Docker it is usually /data/config.yaml")
	}
	return hints
}

func (d *doctor) checkMQTT(cfg *config.Config, src config.Source) {
	host, port := cfg.MQTT.Host, cfg.MQTT.Port
	origin := "from config file"
	for _, e := range src.Env {
		if e == "LB_MQTT_HOST" {
			origin = "from LB_MQTT_HOST"
		}
	}
	d.info("mqtt broker: %s (%s)", net.JoinHostPort(host, strconv.Itoa(port)), origin)

	if host == installerExampleHost {
		d.warn("mqtt.host is %s, the example value written by the installer", installerExampleHost)
		d.info("if you set another broker, doctor is reading a different file than the one you edited (see \"config file\" above)")
	}

	label := "mqtt reach " + net.JoinHostPort(host, strconv.Itoa(port))
	if err := d.reach(host, port); err != nil {
		d.fail(label, err, networkHints(host, port, err, d.inContainer)...)
		return
	}
	d.ok("%s", label)
}

// reach resolves the host (unless it is an IP) and opens a TCP connection.
// A full MQTT CONNECT is not needed to tell "wrong name" from "wrong port".
func (d *doctor) reach(host string, port int) error {
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()

	if net.ParseIP(host) == nil {
		lookup := d.lookupHost
		if lookup == nil {
			lookup = net.DefaultResolver.LookupHost
		}
		if _, err := lookup(ctx, host); err != nil {
			return err
		}
	}
	dial := d.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return conn.Close()
}

// networkHints turns a resolve/connect error into next steps.
func networkHints(host string, port int, err error, inContainer bool) []string {
	var hints []string
	var dnsErr *net.DNSError
	shortName := net.ParseIP(host) == nil && !strings.Contains(host, ".")

	switch {
	case errors.As(err, &dnsErr):
		if shortName && !inContainer {
			hints = append(hints,
				fmt.Sprintf("%q looks like a Docker service name; it only resolves inside that Docker network", host),
				"run doctor in the bridge container (docker compose run --rm lumen-bridge doctor --config /data/config.yaml)",
				"or, if the bridge runs on the host, use the broker's IP (or localhost if its port is published)")
		} else if shortName {
			hints = append(hints, fmt.Sprintf("%q is not on this container's Docker network; put the bridge and the broker on the same network", host))
		} else {
			hints = append(hints, "check the hostname, or use the broker's IP address")
		}
	case errors.Is(err, syscall.ECONNREFUSED):
		if isLoopback(host) && inContainer {
			hints = append(hints, "inside a container, localhost is the container itself; use the broker's service name or IP")
		} else {
			hints = append(hints, fmt.Sprintf("the host answered but nothing listens on port %d; check the port, or that Docker publishes it (-p %d:%d)", port, port, port))
		}
	case isTimeout(err):
		hints = append(hints, "no answer: wrong IP, host down, or a firewall drops the traffic")
	}
	return hints
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

func (d *doctor) checkDeviceToken(cfg *config.Config) {
	stored, err := relay.LoadDeviceToken(cfg.Relay.DeviceTokenPath)
	if err != nil {
		var hints []string
		if errors.Is(err, os.ErrPermission) {
			hints = append(hints, "run as the service user: sudo -u lumen-bridge lumen-bridge doctor")
		}
		d.fail("device token", err, hints...)
		return
	}
	if stored == nil {
		d.warn("device token: missing at %s", cfg.Relay.DeviceTokenPath)
		d.info("generate a code in Lumen → Paired Bridges, then run `lumen-bridge pair --code <6-digit>`")
		return
	}
	d.ok("device token: present")
	if stored.UserRef != "" {
		d.info("user ref: %s", stored.UserRef)
	}
}

func (d *doctor) checkRelay(cfg *config.Config) {
	target := cfg.Relay.URL + "/health"
	label := "relay " + target
	status, err := d.get(target)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("HTTP %d", status)
	}
	if err != nil {
		d.fail(label, err, d.urlHints(target, err)...)
		return
	}
	d.ok("%s", label)
}

func (d *doctor) checkFrigate(cfg *config.Config) {
	if cfg.Frigate.BaseURL == "" {
		d.info("frigate: skipped (frigate.base_url not set; snapshot and clip upload disabled)")
		return
	}
	base := strings.TrimRight(cfg.Frigate.BaseURL, "/")
	label := "frigate " + base
	status, err := d.get(base + "/api/version")
	switch {
	case err != nil:
		d.fail(label, err, d.urlHints(base, err)...)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		d.warn("%s: HTTP %d, reachable but needs login; use the unauthenticated port (5000) on your LAN", label, status)
	case status >= 400:
		d.warn("%s: HTTP %d, check that base_url points at Frigate", label, status)
	default:
		d.ok("%s", label)
	}
}

func (d *doctor) get(target string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	client := d.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

func (d *doctor) urlHints(target string, err error) []string {
	u, perr := url.Parse(target)
	if perr != nil || u.Hostname() == "" {
		return nil
	}
	port := 443
	if u.Scheme == "http" {
		port = 80
	}
	if p, perr := strconv.Atoi(u.Port()); perr == nil {
		port = p
	}
	return networkHints(u.Hostname(), port, err, d.inContainer)
}

// inContainer reports whether doctor runs inside Docker, Podman or similar.
func inContainer() bool {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	if os.Getenv("container") != "" {
		return true
	}
	raw, err := os.ReadFile(filepath.Join("/proc", "1", "cgroup"))
	if err != nil {
		return false
	}
	s := string(raw)
	return strings.Contains(s, "docker") || strings.Contains(s, "kubepods") || strings.Contains(s, "containerd")
}
