// Package config loads the bridge runtime configuration from a YAML file
// (default: /etc/lumen-bridge/config.yaml or ./config.yaml) with env-var
// overrides for every field. Env vars take precedence over file values so
// container deployments can compose the config without writing a file at
// all.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	MQTT     MQTTConfig     `yaml:"mqtt"`
	Relay    RelayConfig    `yaml:"relay"`
	CloudKit CloudKitConfig `yaml:"cloudkit"` // deprecated, retained for legacy config files
	Frigate  FrigateConfig  `yaml:"frigate"`
}

// RelayConfig configures the HTTP client to the Lumen Bridge Relay Worker.
// As of 2026-05-21 this is the primary credential surface — see
// memory/project_relay_proxy_migration.md. Direct-CloudKit fields under
// CloudKitConfig remain in the schema only so existing config files don't
// fail to parse; they're ignored at runtime.
type RelayConfig struct {
	// URL of the Worker, e.g. https://relay.lorislab.fr
	URL string `yaml:"url"`
	// Path to the device-token file produced by `lumen-bridge pair`.
	// Defaults to ~/.config/lumen-bridge/device-token.json.
	DeviceTokenPath string `yaml:"device_token_path"`
}

type MQTTConfig struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	Username    string `yaml:"username"`
	Password    string `yaml:"password"`
	TopicPrefix string `yaml:"topic_prefix"`
	ClientID    string `yaml:"client_id"`
	TLS         bool   `yaml:"tls"` // true → ssl://host:port (typically port 8883)
}

// FrigateConfig is optional. Provide the HTTP base URL (e.g.
// http://frigate.local:5000) to enable clip-MP4 backfill on `end` events.
// Omit to skip clip uploads — events will still be forwarded with their
// snapshot.
type FrigateConfig struct {
	BaseURL string `yaml:"base_url"`
}

type CloudKitConfig struct {
	Container     string `yaml:"container"`
	Environment   string `yaml:"environment"`
	APITokenPath  string `yaml:"api_token_path"`
	UserTokenPath string `yaml:"user_token_path"`
}

// Load resolves a config from `path` (or the standard search locations when
// path == ""), then layers env-var overrides on top. Returns a fully-defaulted
// Config or an error if a *required* field is still missing after both stages.
func Load(path string) (*Config, error) {
	cfg, _, err := LoadWithSource(path)
	return cfg, err
}

// Source says where the effective config came from, so `doctor` can show
// the user which file it read and which env vars replaced its values.
type Source struct {
	// File is the absolute path of the YAML file that was read, or "" when
	// no file was found and the config came from defaults + env only.
	File string
	// Env lists the LB_* variables that replaced a value (names only).
	Env []string
}

// SearchPaths are the locations tried, in order, when no --config is given.
var SearchPaths = []string{
	"./config.yaml",
	"/etc/lumen-bridge/config.yaml",
}

// LoadWithSource is Load plus a description of where the values came from.
// The Source is filled in as far as loading got, even on error.
func LoadWithSource(path string) (*Config, Source, error) {
	cfg := defaults()
	var src Source

	if path == "" {
		path = findConfigFile()
	}
	if path != "" {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		src.File = path
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, src, fmt.Errorf("read %s: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, src, fmt.Errorf("parse %s: %w", path, err)
		}
	}

	src.Env = overlayEnv(cfg)
	cfg.Relay.DeviceTokenPath = ExpandHome(cfg.Relay.DeviceTokenPath)
	cfg.Relay.URL = strings.TrimRight(cfg.Relay.URL, "/")

	if err := cfg.validate(); err != nil {
		return nil, src, err
	}
	return cfg, src, nil
}

// DefaultRelayURL is the production Lumen Bridge Relay Worker.
const DefaultRelayURL = "https://relay.lorislab.fr"

// DefaultDeviceTokenPath is ~/.config/lumen-bridge/device-token.json.
func DefaultDeviceTokenPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "lumen-bridge", "device-token.json")
}

// ExpandHome turns a leading "~/" into the current user's home dir.
// YAML values are not shell-expanded, so without this a literal "~"
// directory would be used.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func defaults() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		MQTT: MQTTConfig{
			Port:        1883,
			TopicPrefix: "frigate",
			ClientID:    "lumen-bridge-linux",
		},
		Relay: RelayConfig{
			URL:             DefaultRelayURL,
			DeviceTokenPath: DefaultDeviceTokenPath(),
		},
		CloudKit: CloudKitConfig{
			Container:     "iCloud.com.lorislabapp.lumenbridge",
			Environment:   "production",
			UserTokenPath: filepath.Join(home, ".config", "lumen-bridge", "token.json"),
		},
	}
}

func findConfigFile() string {
	for _, p := range SearchPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// overlayEnv applies LB_* overrides and returns the names of the ones used.
func overlayEnv(c *Config) []string {
	var applied []string
	str := func(name string, dst *string) {
		if v := os.Getenv(name); v != "" {
			*dst = v
			applied = append(applied, name)
		}
	}
	str("LB_MQTT_HOST", &c.MQTT.Host)
	if v := os.Getenv("LB_MQTT_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MQTT.Port = n
			applied = append(applied, "LB_MQTT_PORT")
		}
	}
	str("LB_MQTT_USERNAME", &c.MQTT.Username)
	str("LB_MQTT_PASSWORD", &c.MQTT.Password)
	str("LB_MQTT_TOPIC_PREFIX", &c.MQTT.TopicPrefix)
	str("LB_MQTT_CLIENT_ID", &c.MQTT.ClientID)
	if v := os.Getenv("LB_MQTT_TLS"); v != "" {
		c.MQTT.TLS = v == "true" || v == "1" || v == "yes"
		applied = append(applied, "LB_MQTT_TLS")
	}
	str("LB_FRIGATE_BASE_URL", &c.Frigate.BaseURL)
	str("LB_CK_CONTAINER", &c.CloudKit.Container)
	str("LB_CK_ENVIRONMENT", &c.CloudKit.Environment)
	str("LB_CK_API_TOKEN_PATH", &c.CloudKit.APITokenPath)
	str("LB_CK_USER_TOKEN_PATH", &c.CloudKit.UserTokenPath)
	str("LB_RELAY_URL", &c.Relay.URL)
	str("LB_RELAY_DEVICE_TOKEN_PATH", &c.Relay.DeviceTokenPath)
	return applied
}

func (c *Config) validate() error {
	if c.MQTT.Host == "" {
		return fmt.Errorf("mqtt.host is required (set in config.yaml or via LB_MQTT_HOST)")
	}
	if c.Relay.URL == "" {
		return fmt.Errorf("relay.url is required (default: https://relay.lorislab.fr)")
	}
	if c.Relay.DeviceTokenPath == "" {
		return fmt.Errorf("relay.device_token_path is required")
	}
	// CloudKit fields are no longer validated — left in the struct for
	// legacy config compatibility but ignored at runtime.
	return nil
}
