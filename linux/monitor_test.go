//go:build linux

package main

import (
	"testing"
	"time"
)

// resetMonitorState gives each test a clean hotMap and a fake exe resolver so
// nothing depends on the real /proc.
func resetMonitorState(t *testing.T, exes map[int]string) {
	t.Helper()
	hotMu.Lock()
	hotMap = map[int]*HotProcess{}
	hotMu.Unlock()

	orig := resolveExeName
	resolveExeName = func(pid int) string { return exes[pid] }
	t.Cleanup(func() {
		resolveExeName = orig
		hotMu.Lock()
		hotMap = map[int]*HotProcess{}
		hotMu.Unlock()
	})
}

func testCfg() Config {
	cfg := defaultConfig()
	cfg.CPUThreshold = 80
	cfg.KillDuration = 60
	return cfg
}

// own returns an entry owned by the test user, with a normal parent.
func own(pid int, name string, cpu float64) processEntry {
	return processEntry{PID: pid, PPID: 1000, Name: name, CPU: cpu, UID: selfUID, StartTime: 42}
}

func runEvaluate(entries []processEntry, cfg Config, protectPID int, now time.Time) []*HotProcess {
	hotMu.Lock()
	defer hotMu.Unlock()
	return evaluate(entries, cfg, protectPID, now)
}

func hotCount() int {
	hotMu.Lock()
	defer hotMu.Unlock()
	return len(hotMap)
}

func TestIsProtected(t *testing.T) {
	for _, name := range []string{
		"systemd", "Xorg", "xorg", "gnome-shell", "kwin_wayland", "Hyprland",
		"pipewire", "systemd-journald", "systemd-logind",
		"gnome-session-b", // kernel-truncated "gnome-session-binary"
		"gnome-session-binary",
	} {
		if !isProtected(name) {
			t.Errorf("%q should be protected", name)
		}
	}
	for _, name := range []string{"chrome", "node", "python3", "cc1plus", "systemdx"} {
		if isProtected(name) {
			t.Errorf("%q should not be protected", name)
		}
	}
}

func TestEvaluate_BelowThresholdNotTracked(t *testing.T) {
	resetMonitorState(t, nil)
	kills := runEvaluate([]processEntry{own(5000, "node", 79.9)}, testCfg(), 0, time.Now())
	if len(kills) != 0 || hotCount() != 0 {
		t.Errorf("below-threshold process should be ignored (kills=%d, hot=%d)", len(kills), hotCount())
	}
}

func TestEvaluate_KillsOnlyAfterDuration(t *testing.T) {
	resetMonitorState(t, nil)
	cfg := testCfg()
	t0 := time.Now()
	entries := []processEntry{own(5000, "node", 95)}

	if kills := runEvaluate(entries, cfg, 0, t0); len(kills) != 0 {
		t.Fatal("must not kill on first sighting")
	}
	if kills := runEvaluate(entries, cfg, 0, t0.Add(55*time.Second)); len(kills) != 0 {
		t.Fatal("must not kill before kill_duration elapses")
	}
	kills := runEvaluate(entries, cfg, 0, t0.Add(60*time.Second))
	if len(kills) != 1 || kills[0].PID != 5000 {
		t.Fatalf("expected PID 5000 to be killed at 60s, got %+v", kills)
	}
	if kills[0].StartTime != 42 {
		t.Errorf("start time not carried to the kill: %d", kills[0].StartTime)
	}
	if hotCount() != 0 {
		t.Error("killed process should be removed from hotMap")
	}
}

func TestEvaluate_CooldownResetsTimer(t *testing.T) {
	resetMonitorState(t, nil)
	cfg := testCfg()
	t0 := time.Now()

	runEvaluate([]processEntry{own(5000, "node", 95)}, cfg, 0, t0)
	// Drops below threshold → evicted.
	runEvaluate([]processEntry{own(5000, "node", 10)}, cfg, 0, t0.Add(30*time.Second))
	if hotCount() != 0 {
		t.Fatal("cooled-down process should be evicted")
	}
	// Hot again: the clock restarts, so 60s after t0 is not a kill.
	runEvaluate([]processEntry{own(5000, "node", 95)}, cfg, 0, t0.Add(35*time.Second))
	if kills := runEvaluate([]processEntry{own(5000, "node", 95)}, cfg, 0, t0.Add(65*time.Second)); len(kills) != 0 {
		t.Error("timer should have restarted after cooldown")
	}
}

func TestEvaluate_Exclusions(t *testing.T) {
	late := 2 * time.Minute
	cases := []struct {
		name    string
		entry   processEntry
		exes    map[int]string
		wl      []string
		protect int
	}{
		{name: "safety exclusion", entry: own(5000, "gnome-shell", 99)},
		{name: "systemd helper", entry: own(5000, "systemd-oomd", 99)},
		{name: "whitelisted by comm", entry: own(5000, "node", 99), wl: []string{"Node"}},
		{name: "whitelisted by exe", entry: own(5000, "Web Content", 99),
			exes: map[int]string{5000: "firefox"}, wl: []string{"firefox"}},
		{name: "whitelisted long name vs truncated comm", entry: own(5000, "gnome-terminal-", 99),
			wl: []string{"gnome-terminal-server"}},
		{name: "protected by exe", entry: own(5000, "wrapper", 99), exes: map[int]string{5000: "Xwayland"}},
		{name: "focused app", entry: own(5000, "blender", 99), protect: 5000},
		{name: "self", entry: own(selfPID, "hotfix", 99)},
		{name: "init", entry: own(1, "weird-init", 99)},
		{name: "kernel thread", entry: processEntry{PID: 5000, PPID: 2, Name: "kworker/0:1", CPU: 99, UID: selfUID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetMonitorState(t, tc.exes)
			cfg := testCfg()
			cfg.Whitelist = tc.wl
			t0 := time.Now()
			runEvaluate([]processEntry{tc.entry}, cfg, tc.protect, t0)
			kills := runEvaluate([]processEntry{tc.entry}, cfg, tc.protect, t0.Add(late))
			if len(kills) != 0 || hotCount() != 0 {
				t.Errorf("excluded process was tracked or killed (kills=%d, hot=%d)", len(kills), hotCount())
			}
		})
	}
}

func TestEvaluate_SkipsOtherUsersUnlessRoot(t *testing.T) {
	if selfUID == 0 {
		t.Skip("running as root: other users' processes are fair game")
	}
	resetMonitorState(t, nil)
	e := own(5000, "postgres", 99)
	e.UID = selfUID + 1
	t0 := time.Now()
	runEvaluate([]processEntry{e}, testCfg(), 0, t0)
	if kills := runEvaluate([]processEntry{e}, testCfg(), 0, t0.Add(2*time.Minute)); len(kills) != 0 {
		t.Error("another user's process must not be targeted")
	}
}

// A whitelist entry added while a process is already hot must still spare it.
func TestEvaluate_WhitelistAppliesToAlreadyHot(t *testing.T) {
	resetMonitorState(t, map[int]string{5000: "firefox"})
	cfg := testCfg()
	t0 := time.Now()
	entries := []processEntry{own(5000, "Web Content", 99)}

	runEvaluate(entries, cfg, 0, t0)
	if hotCount() != 1 {
		t.Fatal("expected the process to be tracked")
	}
	cfg.Whitelist = []string{"firefox"}
	if kills := runEvaluate(entries, cfg, 0, t0.Add(2*time.Minute)); len(kills) != 0 {
		t.Error("newly whitelisted process must not be killed")
	}
}

func TestDisplayName(t *testing.T) {
	cases := []struct{ comm, exe, want string }{
		{"node", "", "node"},
		{"node", "node", "node"},
		{"gnome-terminal-", "gnome-terminal-server", "gnome-terminal-server"},
		{"Web Content", "firefox", "Web Content (firefox)"},
	}
	for _, c := range cases {
		if got := displayName(c.comm, c.exe); got != c.want {
			t.Errorf("displayName(%q, %q) = %q, want %q", c.comm, c.exe, got, c.want)
		}
	}
}

func TestStartStopMonitor_Idempotent(t *testing.T) {
	configMu.Lock()
	saved := current
	current = Config{Enabled: false} // loop runs but checkProcesses is a no-op
	configMu.Unlock()
	t.Cleanup(func() {
		configMu.Lock()
		current = saved
		configMu.Unlock()
	})

	startMonitor()
	startMonitor()
	stopMonitor()
	stopMonitor()
	startMonitor()
	stopMonitor()
}
