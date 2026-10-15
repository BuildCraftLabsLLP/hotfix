//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateXDG points config and state at a temp dir so tests never touch the
// real ~/.config or ~/.local/state.
func isolateXDG(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	return dir
}

func writeConfigFile(t *testing.T, body string) {
	t.Helper()
	path, err := configPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestConfigPath_UsesXDGConfigHome(t *testing.T) {
	dir := isolateXDG(t)
	path, err := configPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "config", "hotfix", "config.json")
	if path != want {
		t.Errorf("configPath = %q, want %q", path, want)
	}
}

func TestLoadConfig_FirstRunWritesDefaults(t *testing.T) {
	isolateXDG(t)
	cfg := loadConfig()
	if !cfg.Enabled || cfg.CPUThreshold != 80 || cfg.KillDuration != 60 ||
		!cfg.KillOnSleep || !cfg.ProtectActiveApp || cfg.LaunchAtLogin {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	path, _ := configPath()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("defaults should be persisted on first run: %v", err)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	isolateXDG(t)
	in := Config{
		Enabled:      false,
		CPUThreshold: 65,
		KillDuration: 120,
		Whitelist:    []string{"blender", "ffmpeg"},
	}
	if err := saveConfig(in); err != nil {
		t.Fatal(err)
	}
	out := loadConfig()
	if out.Enabled || out.CPUThreshold != 65 || out.KillDuration != 120 ||
		out.KillOnSleep || out.ProtectActiveApp {
		t.Errorf("round trip mismatch: %+v", out)
	}
	if len(out.Whitelist) != 2 || out.Whitelist[0] != "blender" || out.Whitelist[1] != "ffmpeg" {
		t.Errorf("whitelist mismatch: %v", out.Whitelist)
	}
	path, _ := configPath()
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("temp file left behind after save")
	}
}

// A hand-written config listing only some keys keeps defaults for the rest.
func TestReadConfigFile_MissingFieldsKeepDefaults(t *testing.T) {
	isolateXDG(t)
	writeConfigFile(t, `{"whitelist": ["blender"]}`)
	cfg, err := readConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || !cfg.ProtectActiveApp || !cfg.KillOnSleep ||
		cfg.CPUThreshold != 80 || cfg.KillDuration != 60 {
		t.Errorf("absent fields should default: %+v", cfg)
	}
	if len(cfg.Whitelist) != 1 || cfg.Whitelist[0] != "blender" {
		t.Errorf("whitelist: %v", cfg.Whitelist)
	}
}

func TestReadConfigFile_ClampsInvalidValues(t *testing.T) {
	isolateXDG(t)
	writeConfigFile(t, `{"cpu_threshold": 250, "kill_duration": 1, "whitelist": null}`)
	cfg, err := readConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CPUThreshold != 80 || cfg.KillDuration != 60 {
		t.Errorf("invalid values should reset to defaults: %+v", cfg)
	}
	if cfg.Whitelist == nil {
		t.Error("whitelist must never be nil")
	}
}

func TestReadConfigFile_MalformedIsAnError(t *testing.T) {
	isolateXDG(t)
	writeConfigFile(t, `{"enabled": tru`)
	if _, err := readConfigFile(); err == nil {
		t.Fatal("malformed JSON must be reported, not silently defaulted")
	}
	// loadConfig still falls back to defaults — without overwriting the file.
	if cfg := loadConfig(); !cfg.Enabled || cfg.CPUThreshold != 80 {
		t.Errorf("loadConfig fallback: %+v", cfg)
	}
	path, _ := configPath()
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "tru") {
		t.Error("a malformed config file must be left for the user to fix")
	}
}

func TestValidateConfig(t *testing.T) {
	ok := defaultConfig()
	if err := validateConfig(ok); err != nil {
		t.Errorf("defaults should validate: %v", err)
	}
	for _, bad := range []Config{
		{CPUThreshold: 0, KillDuration: 60},
		{CPUThreshold: 101, KillDuration: 60},
		{CPUThreshold: 80, KillDuration: 4},
	} {
		if err := validateConfig(bad); err == nil {
			t.Errorf("expected validation error for %+v", bad)
		}
	}
}

// The autostart entry, not the JSON, decides launch_at_login.
func TestLaunchAtLogin_AutostartFileIsSourceOfTruth(t *testing.T) {
	isolateXDG(t)
	writeConfigFile(t, `{"launch_at_login": true}`)
	if cfg := loadConfig(); cfg.LaunchAtLogin {
		t.Error("launch_at_login must be false without an autostart entry")
	}

	if err := setLaunchAtLogin(true); err != nil {
		t.Fatal(err)
	}
	writeConfigFile(t, `{"launch_at_login": false}`)
	if cfg := loadConfig(); !cfg.LaunchAtLogin {
		t.Error("launch_at_login must be true while the autostart entry exists")
	}
}

func TestSetLaunchAtLogin_WritesAndRemovesEntry(t *testing.T) {
	isolateXDG(t)
	path, _ := autostartPath()

	if launchAtLoginEnabled() {
		t.Fatal("should start disabled")
	}
	if err := setLaunchAtLogin(true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("autostart entry not written: %v", err)
	}
	exe, _ := os.Executable()
	if !strings.Contains(string(data), "Exec="+desktopQuote(exe)+"\n") {
		t.Errorf("Exec line should point at the running binary:\n%s", data)
	}
	if !launchAtLoginEnabled() {
		t.Error("should report enabled after writing the entry")
	}

	if err := setLaunchAtLogin(false); err != nil {
		t.Fatal(err)
	}
	if launchAtLoginEnabled() {
		t.Error("should report disabled after removal")
	}
	// Removing twice is fine.
	if err := setLaunchAtLogin(false); err != nil {
		t.Errorf("second removal should be a no-op: %v", err)
	}
}

func TestLaunchAtLoginEnabled_HiddenEntryIsDisabled(t *testing.T) {
	isolateXDG(t)
	path, _ := autostartPath()
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	_ = os.WriteFile(path, []byte("[Desktop Entry]\nType=Application\nExec=hotfix\nHidden=true\n"), 0644)
	if launchAtLoginEnabled() {
		t.Error("Hidden=true means the desktop disabled the entry")
	}
}

func TestDesktopQuote(t *testing.T) {
	cases := map[string]string{
		"/home/me/.local/bin/hotfix":  `"/home/me/.local/bin/hotfix"`,
		"/home/my user/bin/hotfix":    `"/home/my user/bin/hotfix"`,
		`/odd/"q"/$x/` + "`" + `/a\b`: `"/odd/\"q\"/\$x/\` + "`" + `/a\\b"`,
		"/pct/100%/hotfix":            `"/pct/100%%/hotfix"`,
	}
	for in, want := range cases {
		if got := desktopQuote(in); got != want {
			t.Errorf("desktopQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestStateDir_HonorsXDGStateHome(t *testing.T) {
	dir := isolateXDG(t)
	if got, want := stateDir(), filepath.Join(dir, "state", "hotfix"); got != want {
		t.Errorf("stateDir = %q, want %q", got, want)
	}
	if got, want := logFilePath(), filepath.Join(dir, "state", "hotfix", "hotfix.log"); got != want {
		t.Errorf("logFilePath = %q, want %q", got, want)
	}

	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", dir)
	if got, want := stateDir(), filepath.Join(dir, ".local", "state", "hotfix"); got != want {
		t.Errorf("default stateDir = %q, want %q", got, want)
	}
}
