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

	"gopkg.in/yaml.v3"
)

type Config struct {
	MQTT     MQTTConfig     `yaml:"mqtt"`
	CloudKit CloudKitConfig `yaml:"cloudkit"`
}

type MQTTConfig struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	Username    string `yaml:"username"`
	Password    string `yaml:"password"`
	TopicPrefix string `yaml:"topic_prefix"`
	ClientID    string `yaml:"client_id"`
}

type CloudKitConfig struct {
	Container      string `yaml:"container"`
	Environment    string `yaml:"environment"`
	APITokenPath   string `yaml:"api_token_path"`
	UserTokenPath  string `yaml:"user_token_path"`
}

// Load resolves a config from `path` (or the standard search locations when
// path == ""), then layers env-var overrides on top. Returns a fully-defaulted
// Config or an error if a *required* field is still missing after both stages.
func Load(path string) (*Config, error) {
	cfg := defaults()

	if path == "" {
		path = findConfigFile()
	}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}

	overlayEnv(cfg)

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaults() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		MQTT: MQTTConfig{
			Port:        1883,
			TopicPrefix: "frigate",
			ClientID:    "lumen-bridge-linux",
		},
		CloudKit: CloudKitConfig{
			Container:     "iCloud.com.lorislabapp.lumenbridge",
			Environment:   "production",
			UserTokenPath: filepath.Join(home, ".config", "lumen-bridge", "token.json"),
		},
	}
}

func findConfigFile() string {
	for _, p := range []string{
		"./config.yaml",
		"/etc/lumen-bridge/config.yaml",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func overlayEnv(c *Config) {
	if v := os.Getenv("LB_MQTT_HOST"); v != "" {
		c.MQTT.Host = v
	}
	if v := os.Getenv("LB_MQTT_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MQTT.Port = n
		}
	}
	if v := os.Getenv("LB_MQTT_USERNAME"); v != "" {
		c.MQTT.Username = v
	}
	if v := os.Getenv("LB_MQTT_PASSWORD"); v != "" {
		c.MQTT.Password = v
	}
	if v := os.Getenv("LB_MQTT_TOPIC_PREFIX"); v != "" {
		c.MQTT.TopicPrefix = v
	}
	if v := os.Getenv("LB_MQTT_CLIENT_ID"); v != "" {
		c.MQTT.ClientID = v
	}
	if v := os.Getenv("LB_CK_CONTAINER"); v != "" {
		c.CloudKit.Container = v
	}
	if v := os.Getenv("LB_CK_ENVIRONMENT"); v != "" {
		c.CloudKit.Environment = v
	}
	if v := os.Getenv("LB_CK_API_TOKEN_PATH"); v != "" {
		c.CloudKit.APITokenPath = v
	}
	if v := os.Getenv("LB_CK_USER_TOKEN_PATH"); v != "" {
		c.CloudKit.UserTokenPath = v
	}
}

func (c *Config) validate() error {
	if c.MQTT.Host == "" {
		return fmt.Errorf("mqtt.host is required (set in config.yaml or via LB_MQTT_HOST)")
	}
	if c.CloudKit.Container == "" {
		return fmt.Errorf("cloudkit.container is required")
	}
	if c.CloudKit.Environment != "production" && c.CloudKit.Environment != "development" {
		return fmt.Errorf("cloudkit.environment must be 'production' or 'development', got %q", c.CloudKit.Environment)
	}
	return nil
}
