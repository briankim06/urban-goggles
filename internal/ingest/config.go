package ingest

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// FeedConfig describes a single GTFS-RT endpoint to poll.
type FeedConfig struct {
	Name         string        `yaml:"name"`
	URL          string        `yaml:"url"`
	PollInterval time.Duration `yaml:"poll_interval"`
	Routes       []string      `yaml:"routes"`
}

// Config is the top-level YAML structure consumed by cmd/ingestor.
type Config struct {
	AgencyID               string        `yaml:"agency_id"`
	Feeds                  []FeedConfig  `yaml:"feeds"`
	PollingDefaultInterval time.Duration `yaml:"polling_default_interval"`
	KafkaBrokers           []string      `yaml:"kafka_brokers"`
	KafkaTopic             string        `yaml:"kafka_topic"`
}

// LoadConfig reads and parses a YAML config file from disk.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.PollingDefaultInterval == 0 {
		cfg.PollingDefaultInterval = 15 * time.Second
	}
	if cfg.KafkaTopic == "" {
		cfg.KafkaTopic = "delay-events"
	}
	for i := range cfg.Feeds {
		if cfg.Feeds[i].PollInterval == 0 {
			cfg.Feeds[i].PollInterval = cfg.PollingDefaultInterval
		}
	}
	return &cfg, nil
}
