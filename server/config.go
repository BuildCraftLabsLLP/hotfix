package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Modes. A fresh install watches only: it logs what it WOULD kill so admins
// can tune thresholds and exclusions before switching to enforce.
const (
	ModeMonitor = "monitor"
	ModeEnforce = "enforce"
)

// Config is hotfixd's settings file (config.json in the config directory).
type Config struct {
	Mode             string   `json:"mode"`               // "monitor" (log only) or "enforce" (kill)
	CPUThreshold     float64  `json:"cpu_threshold"`      // percent of ONE core; a process above it is "hot"
	KillDuration     float64  `json:"kill_duration"`      // seconds a process must stay hot before it's killed
	TermGraceSeconds float64  `json:"term_grace_seconds"` // SIGTERM → SIGKILL delay (Linux/macOS; Windows kills at once)
	Whitelist        []string `json:"whitelist"`          // process names never killed
	ProtectUsers     []string `json:"protect_users"`      // processes of these users are never killed (e.g. postgres)
	OnlyUsers        []string `json:"only_users"`         // if set, only these users' processes are candidates
	SkipContainers   bool     `json:"skip_containers"`    // never kill processes inside containers (Linux)
	WebhookURL       string   `json:"webhook_url"`        // POSTed a JSON event on every kill / would-kill
	HTTPSProxy       string   `json:"https_proxy"`        // proxy for webhook and license calls
	LicenseKeyFile   string   `json:"license_key_file"`   // file holding a license key to activate on start
}

func defaultConfig() Config {
	return Config{
		Mode:             ModeMonitor,
		CPUThreshold:     90,
		KillDuration:     300,
		TermGraceSeconds: 10,
		Whitelist:        []string{},
		ProtectUsers:     []string{},
		OnlyUsers:        []string{},
	}
}

// maxThreshold allows thresholds above 100% for multi-threaded work: CPU is
// measured as a percent of one core.
const maxThreshold = 100 * 256

func validateConfig(c Config) error {
	switch c.Mode {
	case ModeMonitor, ModeEnforce:
	default:
		return fmt.Errorf("mode must be %q or %q", ModeMonitor, ModeEnforce)
	}
	if c.CPUThreshold < 1 || c.CPUThreshold > maxThreshold {
		return fmt.Errorf("cpu_threshold must be between 1 and %d (percent of one core)", maxThreshold)
	}
	if c.KillDuration < 5 {
		return errors.New("kill_duration must be at least 5 seconds")
	}
	if c.TermGraceSeconds < 0 || c.TermGraceSeconds > 300 {
		return errors.New("term_grace_seconds must be between 0 and 300")
	}
	for _, u := range []struct{ name, val string }{{"webhook_url", c.WebhookURL}, {"https_proxy", c.HTTPSProxy}} {
		if u.val == "" {
			continue
		}
		p, err := url.Parse(u.val)
		if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			return fmt.Errorf("%s must be an http(s) URL", u.name)
		}
	}
	return nil
}

func configPath() string { return filepath.Join(configDir(), "config.json") }

// readConfig reads and validates the config file. Missing keys keep their
// defaults; a missing file is the default config.
func readConfig(path string) (Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return defaultConfig(), fmt.Errorf("%s: %w", path, err)
	}
	for _, l := range []*[]string{&cfg.Whitelist, &cfg.ProtectUsers, &cfg.OnlyUsers} {
		if *l == nil {
			*l = []string{}
		}
	}
	if err := validateConfig(cfg); err != nil {
		return defaultConfig(), fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// writeConfig validates and saves atomically (temp file + rename), so the
// service never reads a half-written file.
func writeConfig(path string, cfg Config) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// setConfigValue applies `hotfixd config set <key> <value>`. Lists take a
// comma-separated value ("" clears them).
func setConfigValue(cfg *Config, key, value string) error {
	list := func() []string {
		out := []string{}
		for _, s := range strings.Split(value, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	num := func() (float64, error) {
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, fmt.Errorf("%s needs a number", key)
		}
		return f, nil
	}
	setNum := func(dst *float64) error {
		f, err := num()
		if err == nil {
			*dst = f
		}
		return err
	}
	var err error
	switch key {
	case "mode":
		cfg.Mode = strings.ToLower(value)
	case "cpu_threshold":
		err = setNum(&cfg.CPUThreshold)
	case "kill_duration":
		err = setNum(&cfg.KillDuration)
	case "term_grace_seconds":
		err = setNum(&cfg.TermGraceSeconds)
	case "whitelist":
		cfg.Whitelist = list()
	case "protect_users":
		cfg.ProtectUsers = list()
	case "only_users":
		cfg.OnlyUsers = list()
	case "skip_containers":
		var b bool
		if b, err = strconv.ParseBool(value); err == nil {
			cfg.SkipContainers = b
		}
	case "webhook_url":
		cfg.WebhookURL = value
	case "https_proxy":
		cfg.HTTPSProxy = value
	case "license_key_file":
		cfg.LicenseKeyFile = value
	default:
		return fmt.Errorf("unknown setting %q", key)
	}
	if err != nil {
		return err
	}
	return validateConfig(*cfg)
}

// configStore holds the live config and reloads it when the file changes
// (an admin's edit, or `hotfixd config set`). A broken file is logged and
// ignored, never replaced: the last good config stays in effect.
type configStore struct {
	path string

	mu    sync.RWMutex
	cfg   Config
	stamp time.Time
}

func newConfigStore(path string) (*configStore, error) {
	s := &configStore{path: path}
	cfg, err := readConfig(path)
	if err != nil {
		return nil, err
	}
	s.cfg = cfg
	s.stamp = modTime(path)
	return s, nil
}

func (s *configStore) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Reload rereads the file if it changed since the last read (or if force).
// It reports whether a new config took effect.
func (s *configStore) Reload(force bool) (bool, error) {
	stamp := modTime(s.path)
	s.mu.RLock()
	same := stamp.Equal(s.stamp)
	s.mu.RUnlock()
	if same && !force {
		return false, nil
	}
	cfg, err := readConfig(s.path)
	s.mu.Lock()
	s.stamp = stamp
	if err == nil {
		s.cfg = cfg
	}
	s.mu.Unlock()
	return err == nil, err
}

func modTime(path string) time.Time {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}
