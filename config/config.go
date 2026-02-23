package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	WebSocket WebSocketConfig `yaml:"websocket"`
	Server    ServerConfig    `yaml:"server"`
	Logging   LoggingConfig   `yaml:"logging"`
}

type WebSocketConfig struct {
	MaxSessions int           `yaml:"max_sessions"`
	IdleTimeout time.Duration `yaml:"idle_timeout"`
}

type ServerConfig struct {
	Addr string `yaml:"addr"`
}

type LoggingConfig struct {
	Level string `yaml:"level"`
}

func Default() *Config {
	return &Config{
		WebSocket: WebSocketConfig{
			MaxSessions: 100,
			IdleTimeout: 1 * time.Hour,
		},
		Server: ServerConfig{
			Addr: ":8080",
		},
		Logging: LoggingConfig{
			Level: "info",
		},
	}
}

// Load config from yml
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	return cfg, nil
}
