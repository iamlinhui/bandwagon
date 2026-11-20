package dashboard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"bandwagon/internal/webssh"
)

// defaultSSHUser is used when an ssh block supplies credentials but no user.
const defaultSSHUser = "root"

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
	// SSH carries this instance's terminal credentials. Per instance on purpose:
	// machines rarely share a key, and an absent block simply means "no SSH
	// terminal for this one" (the KiwiVM Basic Shell still works).
	SSH SSHConfig `json:"ssh,omitempty"`
}

// SSHConfig is the credential/address set for one instance's web terminal.
//
// Host and Port are optional: the dashboard already learns the address and the
// (per-VM, non-standard) SSH port from the KiwiVM API, so normally you only
// supply the user plus a password or a private key.
type SSHConfig struct {
	// Host overrides the dial address. Empty = first IP reported by KiwiVM.
	Host string `json:"host,omitempty"`
	// Port overrides the dial port. 0 = ssh_port from getLiveServiceInfo, else 22.
	Port int `json:"port,omitempty"`
	// User is the login name. Default "root".
	User string `json:"user,omitempty"`
	// Password authenticates with a password.
	Password string `json:"password,omitempty"`
	// PrivateKey is an inline PEM block, e.g. "-----BEGIN OPENSSH PRIVATE KEY-----…".
	PrivateKey string `json:"private_key,omitempty"`
	// PrivateKeyFile reads the PEM from disk instead of inlining it. A relative
	// path is resolved against the directory holding config.json. The file
	// contents are loaded into PrivateKey at startup, so a bad path fails fast.
	PrivateKeyFile string `json:"private_key_file,omitempty"`
	// Passphrase decrypts an encrypted private key.
	Passphrase string `json:"passphrase,omitempty"`
	// HostKeyFingerprint pins the server host key ("SHA256:…"). Empty = accept
	// any key but report its fingerprint in the terminal (trust on first use).
	HostKeyFingerprint string `json:"host_key_fingerprint,omitempty"`
}

// Present reports whether this instance has enough credentials to attempt an
// SSH login.
func (s SSHConfig) Present() bool {
	return s.Password != "" || s.PrivateKey != ""
}

// ShellConfig controls the in-dashboard web shell (KiwiVM "Basic shell").
type ShellConfig struct {
	// Enabled turns the web shell on. Every command runs as root inside the
	// VPS, so the feature is opt-in.
	Enabled bool `json:"enabled,omitempty"`
	// TimeoutSeconds bounds a single command. Default 60, clamped to 5..600.
	// KiwiVM itself also gives up on long-running commands.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// Timeout returns the per-command deadline, clamped to a sane range.
func (s ShellConfig) Timeout() time.Duration {
	sec := s.TimeoutSeconds
	if sec <= 0 {
		sec = defaultShellTimeoutSec
	}
	if sec < minShellTimeoutSec {
		sec = minShellTimeoutSec
	}
	if sec > maxShellTimeoutSec {
		sec = maxShellTimeoutSec
	}
	return time.Duration(sec) * time.Second
}

const (
	defaultShellTimeoutSec = 60
	minShellTimeoutSec     = 5
	maxShellTimeoutSec     = 600
)

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
	// Shell enables the optional web shell (see ShellConfig).
	Shell ShellConfig `json:"shell,omitempty"`
	// AllowedOrigins whitelists browser Origins for the SSH terminal's WebSocket
	// handshake, e.g. ["vps.example.com", "*.example.com"]. Empty keeps the
	// default same-origin check; ["*"] disables it. Only needed when a reverse
	// proxy rewrites the Host header.
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
	// Instances is the list of VPS instances to monitor.
	Instances []InstanceConfig `json:"instances"`
}

// Load reads and validates the JSON config at path. Relative private_key_file
// paths resolve against the directory holding the config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.applyDefaults(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults(configDir string) error {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Title == "" {
		c.Title = "VPS看板"
	}
	if c.PollSeconds <= 0 {
		c.PollSeconds = 60
	}
	if c.Shell.TimeoutSeconds <= 0 {
		c.Shell.TimeoutSeconds = defaultShellTimeoutSec
	}
	for i := range c.Instances {
		ic := &c.Instances[i]
		if ic.ID == "" {
			ic.ID = ic.VEID
		}
		if ic.Name == "" {
			ic.Name = ic.VEID
		}
		if err := ic.SSH.resolve(configDir); err != nil {
			return fmt.Errorf("config: instance #%d (%s) ssh: %w", i+1, ic.Name, err)
		}
	}
	return nil
}

// resolve loads a private key from disk and fills in defaults. It runs while the
// config is loaded so a bad path or an unparsable key fails at startup instead
// of at the first connection attempt.
func (s *SSHConfig) resolve(configDir string) error {
	if s.Port < 0 || s.Port > 65535 {
		return fmt.Errorf("port %d out of range (1-65535)", s.Port)
	}
	if s.PrivateKeyFile != "" && s.PrivateKey == "" {
		p := s.PrivateKeyFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(configDir, p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read private_key_file %q: %w", s.PrivateKeyFile, err)
		}
		s.PrivateKey = string(data)
	}
	if s.Present() && s.User == "" {
		s.User = defaultSSHUser
	}
	return nil
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

		switch {
		case ic.SSH == SSHConfig{}:
			// no terminal for this instance
		case !ic.SSH.Present():
			return fmt.Errorf("config: instance %s: ssh 块里既没有 password 也没有 private_key/private_key_file", ic.Name)
		default:
			if _, err := webssh.ParsePrivateKey(ic.SSH.PrivateKey, ic.SSH.Passphrase); err != nil {
				return fmt.Errorf("config: instance %s: ssh private_key: %w", ic.Name, err)
			}
		}
	}
	return nil
}
