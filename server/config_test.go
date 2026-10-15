package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsAreSafe(t *testing.T) {
	c := defaultConfig()
	if c.Mode != ModeMonitor {
		t.Error("a fresh install must start in monitor (watch-only) mode")
	}
	if err := validateConfig(c); err != nil {
		t.Fatal(err)
	}
}

func TestReadConfigKeepsDefaultsForMissingKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"mode":"enforce","whitelist":["x"]}`), 0o644)
	c, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != ModeEnforce || c.CPUThreshold != 90 || c.KillDuration != 300 || len(c.Whitelist) != 1 {
		t.Fatalf("got %+v", c)
	}
	if c.ProtectUsers == nil {
		t.Error("lists must never be nil")
	}
}

func TestMissingFileIsDefaults(t *testing.T) {
	c, err := readConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || c.Mode != ModeMonitor {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestInvalidConfigsRejected(t *testing.T) {
	bad := []func(*Config){
		func(c *Config) { c.Mode = "kill-everything" },
		func(c *Config) { c.CPUThreshold = 0 },
		func(c *Config) { c.KillDuration = 1 },
		func(c *Config) { c.TermGraceSeconds = -1 },
		func(c *Config) { c.WebhookURL = "ftp://x" },
		func(c *Config) { c.HTTPSProxy = "not a url" },
	}
	for i, f := range bad {
		c := defaultConfig()
		f(&c)
		if validateConfig(c) == nil {
			t.Errorf("case %d: invalid config accepted: %+v", i, c)
		}
	}
	big := defaultConfig()
	big.CPUThreshold = 800 // 8 cores' worth: allowed, CPU is per core
	if err := validateConfig(big); err != nil {
		t.Error(err)
	}
}

func TestSetConfigValue(t *testing.T) {
	c := defaultConfig()
	for _, kv := range [][2]string{
		{"mode", "ENFORCE"}, {"cpu_threshold", "150"}, {"kill_duration", "120"},
		{"whitelist", " ffmpeg, pg_dump ,,"}, {"skip_containers", "true"},
		{"webhook_url", "https://hooks.example.com/x"},
	} {
		if err := setConfigValue(&c, kv[0], kv[1]); err != nil {
			t.Fatalf("%s: %v", kv[0], err)
		}
	}
	if c.Mode != ModeEnforce || c.CPUThreshold != 150 || c.KillDuration != 120 ||
		len(c.Whitelist) != 2 || c.Whitelist[1] != "pg_dump" || !c.SkipContainers {
		t.Fatalf("got %+v", c)
	}
	if setConfigValue(&c, "nope", "1") == nil || setConfigValue(&c, "cpu_threshold", "lots") == nil {
		t.Error("bad key/value accepted")
	}
	if err := setConfigValue(&c, "whitelist", ""); err != nil || len(c.Whitelist) != 0 {
		t.Error("empty value should clear a list")
	}
}

func TestWriteIsAtomicAndValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	bad := defaultConfig()
	bad.Mode = "x"
	if writeConfig(path, bad) == nil {
		t.Fatal("invalid config written")
	}
	good := defaultConfig()
	good.Mode = ModeEnforce
	if err := writeConfig(path, good); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
	if c, _ := readConfig(path); c.Mode != ModeEnforce {
		t.Error("round trip failed")
	}
}

func TestStoreReloadsOnChangeAndKeepsLastGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(path, defaultConfig())
	s, err := newConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed, _ := s.Reload(false); changed {
		t.Error("unchanged file reported as changed")
	}

	c := defaultConfig()
	c.Mode = ModeEnforce
	writeConfig(path, c)
	future := time.Now().Add(time.Minute)
	os.Chtimes(path, future, future)
	if changed, err := s.Reload(false); !changed || err != nil || s.Get().Mode != ModeEnforce {
		t.Fatalf("change not picked up: %v %v %+v", changed, err, s.Get())
	}

	os.WriteFile(path, []byte("{broken"), 0o644)
	later := future.Add(time.Minute)
	os.Chtimes(path, later, later)
	if _, err := s.Reload(false); err == nil {
		t.Error("broken file should report an error")
	}
	if s.Get().Mode != ModeEnforce {
		t.Error("a broken file must not replace the last good config")
	}
}
