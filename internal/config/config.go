package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version   int                   `yaml:"version"`
	Transport TransportConfig       `yaml:"transport"`
	Roles     map[string]RoleConfig `yaml:"roles"`
}

type TransportConfig struct {
	EventSocket   string `yaml:"event_socket"`
	ControlSocket string `yaml:"control_socket"`
}

type RoleConfig struct {
	StablePath   string `yaml:"stable_path"`
	DeviceID     string `yaml:"device_id"`
	Grab         bool   `yaml:"grab"`
	AutoDiscover bool   `yaml:"auto_discover,omitempty"`
}

// Load reads and parses a YAML config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.Roles == nil {
		cfg.Roles = make(map[string]RoleConfig)
	}
	if cfg.Transport.EventSocket == "" {
		cfg.Transport.EventSocket = "/run/inputd/input.sock"
	}
	if cfg.Transport.ControlSocket == "" {
		cfg.Transport.ControlSocket = "/run/inputd/control.sock"
	}
	return &cfg, nil
}

// Save writes the config to a YAML file atomically.
func Save(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return os.Rename(tmp, path)
}

// Default returns a starter config for the known hardware on this NUC.
func Default() *Config {
	return &Config{
		Version: 1,
		Transport: TransportConfig{
			EventSocket:   "/run/inputd/input.sock",
			ControlSocket: "/run/inputd/control.sock",
		},
		Roles: map[string]RoleConfig{
			"primary_input": {
				StablePath: "/dev/input/primary_keypad",
				DeviceID:   "primary_keypad",
				Grab:       false,
			},
			"secondary_input": {
				StablePath: "/dev/input/secondary_keypad",
				DeviceID:   "secondary_keypad",
				Grab:       false,
			},
		},
	}
}
