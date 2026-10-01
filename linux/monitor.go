//go:build linux

package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// safetyExclusions are processes that must NEVER be killed, regardless of
// config: init, the display server / compositor / session, the audio stack and
// the session bus. Killing any of these takes the whole desktop session down.
var safetyExclusions = []string{
	"systemd", "init", "kthreadd",
	"dbus-daemon", "dbus-broker", "dbus-broker-launch",
	"Xorg", "X", "Xwayland",
	"gnome-shell", "gnome-session-binary", "mutter",
	"kwin_x11", "kwin_wayland", "plasmashell", "ksmserver",
	"sway", "Hyprland", "river", "labwc", "wayfire", "weston",
	"cinnamon", "cinnamon-session", "xfwm4", "xfce4-session", "xfce4-panel",
	"mate-session", "marco", "openbox", "i3", "bspwm",
	"gdm", "gdm3", "gdm-x-session", "gdm-wayland-session", "sddm", "lightdm",
	"login", "sshd",
	"pipewire", "pipewire-pulse", "wireplumber", "pulseaudio",
	"xdg-desktop-portal", "gnome-keyring-daemon", "ssh-agent", "gpg-agent",
	"ibus-daemon", "fcitx5",
}

// commMax is the kernel's limit on a task name (TASK_COMM_LEN - 1). Longer
// executable names show up truncated in /proc/<pid>/stat.
const commMax = 15

// nameSet builds a case-insensitive lookup that also contains each name's
// 15-char truncation, so "gnome-session-binary" matches the kernel's
// "gnome-session-b".
func nameSet(names []string) map[string]bool {
	set := make(map[string]bool, 2*len(names))
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		set[n] = true
		if len(n) > commMax {
			set[n[:commMax]] = true
		}
	}
	return set
}

var protectedSet = nameSet(safetyExclusions)

// isProtected reports whether a process must never be killed regardless of user
// config. Case-insensitive; every systemd-* helper is protected by prefix.
func isProtected(name string) bool {
	n := strings.ToLower(name)
	return protectedSet[n] || strings.HasPrefix(n, "systemd-")
}

// HotProcess tracks a process that has been above the CPU threshold.
type HotProcess struct {
	PID       int
	Name      string // display name
	Exe       string // executable file name ("" if unreadable)
	CPU       float64
	HotSince  time.Time
	StartTime uint64 // guards the SIGKILL follow-up against PID reuse
}

var (
	hotMu       sync.Mutex
	hotMap      = map[int]*HotProcess{} // pid → entry
	monitorStop chan struct{}
	monitorOnce sync.Once
	monitorMu   sync.Mutex // guards monitorStop + monitorOnce

	selfPID = os.Getpid()
	selfUID = uint32(os.Getuid())

	// resolveExeName is a seam so tests don't read the real /proc.
	resolveExeName = exeBaseName
)

// killGrace is how long a process gets to exit after SIGTERM before SIGKILL.
const killGrace = 3 * time.Second

// startMonitor launches the background polling goroutine.
func startMonitor() {
	monitorMu.Lock()
	defer monitorMu.Unlock()
	monitorOnce.Do(func() {
		monitorStop = make(chan struct{})
		stop := monitorStop // capture for goroutine
		safeGo("monitor-loop", func() { monitorLoop(stop) })
	})
}

// stopMonitor signals the monitor goroutine to exit and resets state.
func stopMonitor() {
	monitorMu.Lock()
	ch := monitorStop
	monitorOnce = sync.Once{} // allow future restart
	monitorStop = nil
	monitorMu.Unlock()

	if ch != nil {
		close(ch) // closing broadcasts to the goroutine even if it's mid-checkProcesses
	}

	resetHot()
}

// resetHot forgets every tracked hot process and the CPU baseline.
func resetHot() {
	hotMu.Lock()
	hotMap = map[int]*HotProcess{}
	hotMu.Unlock()
	resetSamples()
}

func monitorLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Immediate first check (records the CPU baseline).
	checkProcesses()

	for {
		select {
		case <-ticker.C:
			checkProcesses()
		case <-stop:
			return
		}
	}
}

// checkProcesses samples /proc, evaluates thresholds, and kills when warranted.
func checkProcesses() {
	cfg := getConfig()
	if !cfg.Enabled || !licensed() {
		return
	}

	entries, err := queryProcesses()
	if err != nil {
		logf("monitor: query error: %v", err)
		return
	}

	// Spare the process that owns the focused window — the app the user is
	// actively using — when the setting is on. 0 means "unknown / none".
	protectPID := 0
	if cfg.ProtectActiveApp {
		protectPID = foregroundPID()
	}

	hotMu.Lock()
	defer hotMu.Unlock()
	for _, hp := range evaluate(entries, cfg, protectPID, time.Now()) {
		killProcess(hp)
	}
}

// evaluate updates hotMap from one poll's entries and returns the processes
// that have been hot for at least cfg.KillDuration (already removed from
// hotMap). The caller must hold hotMu.
func evaluate(entries []processEntry, cfg Config, protectPID int, now time.Time) []*HotProcess {
	wl := nameSet(cfg.Whitelist)

	var toKill []*HotProcess
	// Build new set of hot pids so we can evict cooled-down entries.
	hotThisCycle := map[int]bool{}

	for _, e := range entries {
		if e.CPU < cfg.CPUThreshold {
			continue
		}
		// PID 1/2 and kernel threads (children of kthreadd) are never candidates.
		if e.PID <= 2 || e.PPID == 2 || e.PID == selfPID {
			continue
		}
		// Without root we can only signal our own processes; skip the rest
		// rather than fail on every poll.
		if selfUID != 0 && e.UID != selfUID {
			continue
		}
		if protectPID != 0 && e.PID == protectPID {
			continue
		}

		hp, exists := hotMap[e.PID]

		// Match exclusions on both the (possibly truncated) task name and the
		// real executable name, so whitelisting either one works.
		var exe string
		if exists {
			exe = hp.Exe
		} else {
			exe = resolveExeName(e.PID)
		}
		if isProtected(e.Name) || wl[strings.ToLower(e.Name)] {
			continue
		}
		if exe != "" && (isProtected(exe) || wl[strings.ToLower(exe)]) {
			continue
		}

		hotThisCycle[e.PID] = true

		if !exists {
			hp = &HotProcess{
				PID:       e.PID,
				Name:      displayName(e.Name, exe),
				Exe:       exe,
				CPU:       e.CPU,
				HotSince:  now,
				StartTime: e.StartTime,
			}
			hotMap[e.PID] = hp
		} else {
			hp.CPU = e.CPU
		}

		elapsed := now.Sub(hp.HotSince).Seconds()
		logf("monitor: %s (PID %d) at %.1f%% CPU for %.0fs (threshold %.0fs)",
			hp.Name, hp.PID, hp.CPU, elapsed, cfg.KillDuration)

		if elapsed >= cfg.KillDuration {
			toKill = append(toKill, hp)
			delete(hotMap, e.PID)
		}
	}

	// Evict processes that have cooled down.
	for pid := range hotMap {
		if !hotThisCycle[pid] {
			delete(hotMap, pid)
		}
	}

	return toKill
}

// displayName returns a human-friendly name for logs and notifications: the
// full executable name when the task name is just its truncation, otherwise
// "comm (exe)" (e.g. "Web Content (firefox)"). Display-only.
func displayName(comm, exe string) string {
	switch {
	case exe == "" || exe == comm:
		return comm
	case strings.HasPrefix(exe, comm):
		return exe
	default:
		return fmt.Sprintf("%s (%s)", comm, exe)
	}
}

// killProcess sends SIGTERM, then SIGKILL if the process is still around after
// killGrace (a runaway process often ignores or never gets to handle SIGTERM).
func killProcess(hp *HotProcess) {
	logf("monitor: killing %s (PID %d) — %.1f%% CPU for %.0fs",
		hp.Name, hp.PID, hp.CPU, time.Since(hp.HotSince).Seconds())

	if err := syscall.Kill(hp.PID, syscall.SIGTERM); err != nil {
		logf("monitor: SIGTERM failed for PID %d: %v", hp.PID, err)
		return
	}

	logf("monitor: terminated %s (PID %d)", hp.Name, hp.PID)
	notifyKilled(hp.Name, hp.PID, hp.CPU)

	pid, start, name := hp.PID, hp.StartTime, hp.Name
	if start == 0 {
		return
	}
	time.AfterFunc(killGrace, func() {
		// Same PID and same start time ⇒ same process, still alive.
		if processStartTime(pid) != start {
			return
		}
		logf("monitor: %s (PID %d) ignored SIGTERM — sending SIGKILL", name, pid)
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			logf("monitor: SIGKILL failed for PID %d: %v", pid, err)
		}
	})
}

// killAllHot terminates every currently tracked hot process (sleep handler).
func killAllHot() {
	hotMu.Lock()
	defer hotMu.Unlock()
	for pid, hp := range hotMap {
		killProcess(hp)
		delete(hotMap, pid)
	}
}
