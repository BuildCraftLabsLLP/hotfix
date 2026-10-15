//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Config holds all user-configurable settings for Hotfix. The JSON layout is
// identical to the Windows build's config.json.
type Config struct {
	Enabled          bool     `json:"enabled"`
	CPUThreshold     float64  `json:"cpu_threshold"`      // percent of one core, default 80.0
	KillDuration     float64  `json:"kill_duration"`      // seconds before kill, default 60
	KillOnSleep      bool     `json:"kill_on_sleep"`      // kill hot procs on system suspend
	ProtectActiveApp bool     `json:"protect_active_app"` // never kill the focused app
	LaunchAtLogin    bool     `json:"launch_at_login"`    // start automatically at login
	Whitelist        []string `json:"whitelist"`          // user-managed exclusions
}

func defaultConfig() Config {
	return Config{
		Enabled:          true,
		CPUThreshold:     80.0,
		KillDuration:     60.0,
		KillOnSleep:      true,
		ProtectActiveApp: true,
		Whitelist:        []string{},
	}
}

// configPath returns the path to the config file: $XDG_CONFIG_HOME/hotfix/config.json
// (normally ~/.config/hotfix/config.json).
func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hotfix", "config.json"), nil
}

var (
	configMu sync.RWMutex
	current  Config
	// configStamp is the config file's mtime as of our last read or write. The
	// file watcher reloads whenever the on-disk mtime differs, which is how hand
	// edits (the whitelist is edited in the file) take effect without a restart.
	configStamp time.Time

	// applyMu serializes read-modify-write config changes (tray clicks, file
	// reloads) so two concurrent updates can't lose one another.
	applyMu sync.Mutex
)

// validateConfig rejects out-of-range values. Shared by the tray menu path.
func validateConfig(cfg Config) error {
	if cfg.CPUThreshold < 1 || cfg.CPUThreshold > 100 {
		return fmt.Errorf("cpu_threshold must be 1–100")
	}
	if cfg.KillDuration < 5 {
		return fmt.Errorf("kill_duration must be >= 5")
	}
	return nil
}

// readConfigFile reads and sanitizes the config from disk. Fields missing from
// the file keep their defaults. Unlike loadConfig it reports errors, so a
// half-written file seen mid-save never replaces a good in-memory config.
func readConfigFile() (Config, error) {
	path, err := configPath()
	if err != nil {
		return Config{}, err
	}

	if fi, err := os.Stat(path); err == nil {
		configMu.Lock()
		configStamp = fi.ModTime()
		configMu.Unlock()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	cfg := defaultConfig()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}

	// Guard against invalid values.
	if cfg.CPUThreshold < 1 || cfg.CPUThreshold > 100 {
		cfg.CPUThreshold = 80.0
	}
	if cfg.KillDuration < 5 {
		cfg.KillDuration = 60.0
	}
	if cfg.Whitelist == nil {
		cfg.Whitelist = []string{}
	}

	// The XDG autostart entry is the source of truth for this toggle (install.sh
	// or the user may change it outside the app), so always reflect the real
	// state rather than the persisted JSON.
	cfg.LaunchAtLogin = launchAtLoginEnabled()

	return cfg, nil
}

// loadConfig reads config from disk, falling back to defaults on any error.
func loadConfig() Config {
	cfg, err := readConfigFile()
	if err == nil {
		return cfg
	}

	cfg = defaultConfig()
	cfg.LaunchAtLogin = launchAtLoginEnabled()
	if os.IsNotExist(err) {
		// First run — persist the defaults so there is a file to edit.
		_ = saveConfig(cfg)
	} else {
		logf("config: load error (%v), using defaults", err)
	}
	return cfg
}

// saveConfig persists cfg to disk, creating the directory if needed. The write
// is atomic (temp file + rename) so the file watcher never sees a partial file.
func saveConfig(cfg Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	if fi, err := os.Stat(path); err == nil {
		configMu.Lock()
		configStamp = fi.ModTime()
		configMu.Unlock()
	}
	return nil
}

// getConfig returns the current in-memory config (thread-safe).
func getConfig() Config {
	configMu.RLock()
	defer configMu.RUnlock()
	return current
}

// setConfig atomically replaces the in-memory config and saves to disk.
func setConfig(cfg Config) error {
	configMu.Lock()
	current = cfg
	configMu.Unlock()
	return saveConfig(cfg)
}

// watchConfigFile polls the config file's mtime and reloads it when it was
// changed outside the app (e.g. the user edited the whitelist in an editor).
func watchConfigFile() {
	safeGo("config-watcher", func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			path, err := configPath()
			if err != nil {
				continue
			}
			fi, err := os.Stat(path)
			if err != nil {
				continue
			}
			configMu.RLock()
			changed := !fi.ModTime().Equal(configStamp)
			configMu.RUnlock()
			if changed {
				reloadConfig()
			}
		}
	})
}

// reloadConfig re-reads the config file after an external edit and applies it.
// An unreadable or malformed file is ignored (the in-memory config stays).
func reloadConfig() {
	applyMu.Lock()
	defer applyMu.Unlock()

	cfg, err := readConfigFile()
	if err != nil {
		logf("config: ignoring external edit (%v)", err)
		return
	}
	configMu.Lock()
	current = cfg
	configMu.Unlock()
	logf("config: reloaded from disk (enabled=%v, threshold=%.0f%%, kill_after=%.0fs, whitelist=%d)",
		cfg.Enabled, cfg.CPUThreshold, cfg.KillDuration, len(cfg.Whitelist))
	applyRuntime(cfg)
}
