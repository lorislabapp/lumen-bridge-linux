package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// clearLBEnv unsets every LB_* var for the test so the host env can't leak in.
func clearLBEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "LB_") {
			t.Setenv(name, "")
		}
	}
}

func writeConfig(t *testing.T, mqttHost, relayURL string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf("mqtt:\n  host: %s\n  port: 1883\nrelay:\n  url: %s\n  device_token_path: %s\n",
		mqttHost, relayURL, filepath.Join(dir, "device-token.json"))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func healthyRelay(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type fakeConn struct{ net.Conn }

func (fakeConn) Close() error { return nil }

func newTestDoctor(out *bytes.Buffer, dialErr error, lookupErr error) *doctor {
	return &doctor{
		out: out,
		lookupHost: func(context.Context, string) ([]string, error) {
			if lookupErr != nil {
				return nil, lookupErr
			}
			return []string{"10.0.0.2"}, nil
		},
		dial: func(context.Context, string, string) (net.Conn, error) {
			if dialErr != nil {
				return nil, dialErr
			}
			return fakeConn{}, nil
		},
	}
}

// The reported case: the user's broker is "mqtt", but doctor reads the
// installer's file that still says 192.168.1.50.
func TestDoctor_ShowsWhichFileAndFlagsInstallerPlaceholder(t *testing.T) {
	clearLBEnv(t)
	relay := healthyRelay(t)
	path := writeConfig(t, installerExampleHost, relay.URL)

	var out bytes.Buffer
	code := newTestDoctor(&out, nil, nil).run(path)

	got := out.String()
	for _, want := range []string{
		"config file: " + path + " (from --config)",
		"mqtt broker: 192.168.1.50:1883 (from config file)",
		"example value written by the installer",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n%s", want, got)
		}
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (placeholder is a warning)", code)
	}
}

func TestDoctor_ReportsEnvOverride(t *testing.T) {
	clearLBEnv(t)
	relay := healthyRelay(t)
	path := writeConfig(t, installerExampleHost, relay.URL)
	t.Setenv("LB_MQTT_HOST", "mqtt")

	var out bytes.Buffer
	newTestDoctor(&out, nil, nil).run(path)

	got := out.String()
	if !strings.Contains(got, "env overrides: LB_MQTT_HOST") {
		t.Errorf("env override not listed\n%s", got)
	}
	if !strings.Contains(got, "mqtt broker: mqtt:1883 (from LB_MQTT_HOST)") {
		t.Errorf("mqtt origin not shown\n%s", got)
	}
	if strings.Contains(got, "example value") {
		t.Errorf("placeholder warning shown although env replaced the host\n%s", got)
	}
}

// A failing MQTT check must not stop the relay check, and must exit 1.
func TestDoctor_RunsEveryCheckAndExitsNonZero(t *testing.T) {
	clearLBEnv(t)
	relayHit := false
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relayHit = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(relay.Close)
	path := writeConfig(t, "mqtt", relay.URL)

	var out bytes.Buffer
	dnsErr := &net.DNSError{Err: "no such host", Name: "mqtt", IsNotFound: true}
	code := newTestDoctor(&out, nil, dnsErr).run(path)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !relayHit {
		t.Error("relay check did not run after the MQTT failure")
	}
	got := out.String()
	if !strings.Contains(got, "looks like a Docker service name") {
		t.Errorf("missing Docker name hint\n%s", got)
	}
	if !strings.Contains(got, "doctor found 1 problem(s)") {
		t.Errorf("missing summary\n%s", got)
	}
}

func TestNetworkHints(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}
	refused := &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}

	cases := []struct {
		name        string
		host        string
		err         error
		inContainer bool
		want        string
	}{
		{"short name on host", "mqtt", dnsErr, false, "Docker service name"},
		{"short name in container", "mqtt", dnsErr, true, "same network"},
		{"fqdn", "broker.lan", dnsErr, false, "check the hostname"},
		{"localhost in container", "localhost", refused, true, "the container itself"},
		{"refused on host", "10.0.0.2", refused, false, "nothing listens on port 1883"},
		{"timeout", "10.0.0.2", context.DeadlineExceeded, false, "firewall"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hints := strings.Join(networkHints(tc.host, 1883, tc.err, tc.inContainer), "\n")
			if !strings.Contains(hints, tc.want) {
				t.Errorf("hints %q do not contain %q", hints, tc.want)
			}
		})
	}
	if h := networkHints("x", 1, errors.New("other"), false); len(h) != 0 {
		t.Errorf("unexpected hints for unknown error: %v", h)
	}
}
