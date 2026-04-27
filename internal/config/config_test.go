package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_FromEnvOnly(t *testing.T) {
	resetEnv(t)
	t.Setenv("LB_MQTT_HOST", "broker.lan")
	t.Setenv("LB_MQTT_PORT", "8883")
	t.Setenv("LB_MQTT_USERNAME", "frigate")
	t.Setenv("LB_MQTT_PASSWORD", "secret")
	t.Setenv("LB_CK_ENVIRONMENT", "development")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MQTT.Host != "broker.lan" {
		t.Errorf("host: got %q", cfg.MQTT.Host)
	}
	if cfg.MQTT.Port != 8883 {
		t.Errorf("port: got %d", cfg.MQTT.Port)
	}
	if cfg.MQTT.Username != "frigate" {
		t.Errorf("user: got %q", cfg.MQTT.Username)
	}
	if cfg.MQTT.Password != "secret" {
		t.Errorf("pass: got %q", cfg.MQTT.Password)
	}
	if cfg.CloudKit.Environment != "development" {
		t.Errorf("env: got %q", cfg.CloudKit.Environment)
	}
	// Defaults that were not overridden.
	if cfg.MQTT.TopicPrefix != "frigate" {
		t.Errorf("topicPrefix default: got %q", cfg.MQTT.TopicPrefix)
	}
	if cfg.MQTT.ClientID != "lumen-bridge-linux" {
		t.Errorf("clientID default: got %q", cfg.MQTT.ClientID)
	}
	if cfg.CloudKit.Container != "iCloud.com.lorislabapp.lumenbridge" {
		t.Errorf("container default: got %q", cfg.CloudKit.Container)
	}
}

func TestLoad_YAMLOverlaidByEnv(t *testing.T) {
	resetEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
mqtt:
  host: yaml-host
  port: 1883
  username: yaml-user
cloudkit:
  environment: production
`
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LB_MQTT_PASSWORD", "env-pw")    // not in yaml — picked from env
	t.Setenv("LB_MQTT_USERNAME", "env-user")  // overrides yaml

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MQTT.Host != "yaml-host" {
		t.Errorf("host: got %q (yaml should win when env unset)", cfg.MQTT.Host)
	}
	if cfg.MQTT.Username != "env-user" {
		t.Errorf("user: env should override yaml, got %q", cfg.MQTT.Username)
	}
	if cfg.MQTT.Password != "env-pw" {
		t.Errorf("pass: got %q", cfg.MQTT.Password)
	}
}

func TestValidate_RequiresMQTTHost(t *testing.T) {
	resetEnv(t)
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error when LB_MQTT_HOST unset and no config file")
	}
}

func TestValidate_RejectsBadEnvironment(t *testing.T) {
	resetEnv(t)
	t.Setenv("LB_MQTT_HOST", "broker.lan")
	t.Setenv("LB_CK_ENVIRONMENT", "staging") // not allowed
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error for invalid environment")
	}
}

// resetEnv clears every LB_* env var so tests start from a clean baseline.
// `t.Setenv` restores per-call but doesn't unset variables set by the
// surrounding shell or earlier tests.
func resetEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"LB_MQTT_HOST", "LB_MQTT_PORT", "LB_MQTT_USERNAME", "LB_MQTT_PASSWORD",
		"LB_MQTT_TOPIC_PREFIX", "LB_MQTT_CLIENT_ID",
		"LB_CK_CONTAINER", "LB_CK_ENVIRONMENT",
		"LB_CK_API_TOKEN_PATH", "LB_CK_USER_TOKEN_PATH",
	} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}
