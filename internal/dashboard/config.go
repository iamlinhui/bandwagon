package dashboard

import (
	"encoding/json"
	"fmt"
	"os"
)

// InstanceConfig describes one BandwagonHost VPS to monitor.
type InstanceConfig struct {
	// ID is an optional stable identifier. Defaults to VEID.
	ID string `json:"id,omitempty"`
	// Name is the display label shown on the card.
	Name string `json:"name"`
	// Group is an optional label for filtering/grouping in the UI.
	Group string `json:"group,omitempty"`
	// VEID is the KiwiVM virtual-environment id (numeric string).
	VEID string `json:"veid"`
	// APIKey is the KiwiVM API key (starts with "private_").
	APIKey string `json:"api_key"`
}

// Config is the top-level dashboard configuration.
type Config struct {
	// Listen is the address the HTTP server binds to (default ":8080").
	Listen string `json:"listen,omitempty"`
	// Title is shown in the page header (default "VPS看板").
	Title string `json:"title,omitempty"`
	// Password, when non-empty, gates the dashboard behind a login. Empty = open.
	Password string `json:"password,omitempty"`
	// PollSeconds is the interval between full data refreshes (default 60).
	PollSeconds int `json:"poll_seconds,omitempty"`
	// Instances is the list of VPS instances to monitor.
	Instances []InstanceConfig `json:"instances"`
}

// Load reads and validates the JSON config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Title == "" {
		c.Title = "VPS看板"
	}
	if c.PollSeconds <= 0 {
		c.PollSeconds = 60
	}
	for i := range c.Instances {
		ic := &c.Instances[i]
		if ic.ID == "" {
			ic.ID = ic.VEID
		}
		if ic.Name == "" {
			ic.Name = ic.VEID
		}
	}
}

func (c *Config) validate() error {
	if len(c.Instances) == 0 {
		return fmt.Errorf("config: no instances configured")
	}
	seen := map[string]bool{}
	for i, ic := range c.Instances {
		if ic.VEID == "" || ic.APIKey == "" {
			return fmt.Errorf("config: instance #%d (%s) missing veid or api_key", i+1, ic.Name)
		}
		if seen[ic.ID] {
			return fmt.Errorf("config: duplicate instance id %q", ic.ID)
		}
		seen[ic.ID] = true
	}
	return nil
}
