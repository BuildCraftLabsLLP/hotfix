//go:build linux

package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"
)

// Tray icon: the color flame, same PNG the Windows tray uses. The Linux tray
// (StatusNotifierItem) takes PNG bytes directly, so no ICO wrapping is needed.
//
//go:embed assets/flame32.png
var flamePNG []byte

const defaultTooltip = "Hotfix — by BuildCraft Labs"

// --- Tray state ---
//
// The menu mirrors the Windows app: a status line, Enable Monitoring,
// Settings… (the same settings popover as Windows — see settings_window.go),
// Check for Updates and Quit. Linux adds the subscription items (license.go).
var (
	mStatus *systray.MenuItem
	mToggle *systray.MenuItem

	// Subscription items: Subscribe/Refresh show while the app is locked,
	// Manage while it is subscribed.
	mSubscribe *systray.MenuItem
	mRefresh   *systray.MenuItem
	mManage    *systray.MenuItem

	trayMu        sync.Mutex
	statusTimeout *time.Timer
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v":
			if licenseEnforced() {
				fmt.Println("Hotfix", currentVersion)
			} else {
				fmt.Println("Hotfix", currentVersion, "(dev build: licensing not enforced)")
			}
			return
		case settingsFlag:
			// The settings window runs in its own process (see settings_window.go).
			os.Exit(runSettingsWindow(false))
		case settingsSelfTestFlag:
			os.Exit(runSettingsWindow(true))
		}
	}

	// Set up logging before anything else.
	initLog()

	if !acquireInstanceLock() {
		logf("Hotfix already running; exiting")
		fmt.Fprintln(os.Stderr, "Hotfix is already running.")
		return
	}
	logf("Hotfix starting (version %s)", currentVersion)

	// If a previous run left a crash behind, surface it for one-click reporting.
	reportPendingCrash()

	// Load config.
	cfg := loadConfig()
	configMu.Lock()
	current = cfg
	configMu.Unlock()

	// Hand control to systray — onReady and onExit run on its goroutine.
	systray.Run(onReady, onExit)
}

// instanceLock is held for the life of the process so a second launch (e.g.
// autostart plus a manual start) doesn't run two monitors. It is close-on-exec,
// so the updater's re-exec releases it and the new image takes it again.
var instanceLock *os.File

func acquireInstanceLock() bool {
	dir := stateDir()
	if dir == "" {
		return true // can't lock; don't block startup over it
	}
	_ = os.MkdirAll(dir, 0755)
	f, err := os.OpenFile(filepath.Join(dir, "hotfix.lock"), os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return true
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return false
	}
	instanceLock = f
	return true
}

func onReady() {
	// Set tray icon and tooltip.
	systray.SetIcon(flamePNG)
	systray.SetTitle("Hotfix")
	systray.SetTooltip(defaultTooltip)

	// Build menu (same items and order as the Windows tray).
	mStatus = systray.AddMenuItem("Hotfix — Watching", "")
	mStatus.Disable()
	mSubscribe = systray.AddMenuItem("Subscribe ($1/month)…", "Unlock Hotfix on this computer")
	mRefresh = systray.AddMenuItem("Refresh License", "Check again for an active subscription")
	mSubscribe.Hide()
	mRefresh.Hide()
	systray.AddSeparator()

	mToggle = systray.AddMenuItemCheckbox("Enable Monitoring", "", false)
	systray.AddSeparator()

	mSettings := systray.AddMenuItem("Settings...", "Open Hotfix settings")
	mUpdate := systray.AddMenuItem("Check for Updates", "Check GitHub for a newer release")
	mManage = systray.AddMenuItem("Manage Subscription…", "Update payment details or cancel")
	mManage.Hide()
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit Hotfix")

	// Verify the subscription (a cached license unlocks immediately; the
	// server check continues in the background and locks/unlocks as it lands).
	startLicenseWatcher()

	// Apply the loaded config: starts the monitor if licensed and enabled, and
	// syncs the menu.
	applyRuntime(getConfig())

	// Watch for system suspend (KillOnSleep support).
	watchSleep()

	// Pick up config changes made by the settings window or by hand.
	watchConfigFile()

	// Requests from the settings window process.
	watchSettingsSignals()

	// Begin silent background auto-updates (launch check + periodic poll).
	startAutoUpdater()

	// Click handlers. Each runs through safe(), so a panic in one handler is
	// captured and reported rather than silently killing the tray.
	onClick(mToggle, "toggle", func() { updateConfig(func(c *Config) { c.Enabled = !c.Enabled }) })
	onClick(mSettings, "settings", openSettings)
	onClick(mUpdate, "update", func() { safeGo("update", func() { checkForUpdates(false) }) })
	onClick(mSubscribe, "subscribe", openSubscribePage)
	onClick(mRefresh, "refresh-license", recheckLicenseNow)
	onClick(mManage, "manage-subscription", openManagePage)
	onClick(mQuit, "quit", systray.Quit)
}

func onExit() {
	stopMonitor()
	closeSettingsWindow()
	logf("Hotfix exiting")
	closeLog()
}

// onClick runs fn for every click on item, on a dedicated goroutine.
func onClick(item *systray.MenuItem, name string, fn func()) {
	safeGo("menu-"+name, func() {
		for range item.ClickedCh {
			safe(name, fn)
		}
	})
}

// watchSettingsSignals serves the settings window process, which signals the
// tray process instead of opening a channel to it: SIGUSR1 = check for updates,
// SIGUSR2 = the config file was saved, reload it now.
func watchSettingsSignals() {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGUSR1, syscall.SIGUSR2)
	safeGo("settings-signals", func() {
		for sig := range ch {
			switch sig {
			case syscall.SIGUSR1:
				safeGo("update", func() { checkForUpdates(false) })
			case syscall.SIGUSR2:
				reloadConfig()
			}
		}
	})
}

// updateConfig applies one change from the tray menu to the current config.
func updateConfig(mutate func(*Config)) {
	applyMu.Lock()
	defer applyMu.Unlock()

	cfg := getConfig()
	mutate(&cfg)
	if err := applyConfig(cfg); err != nil {
		logf("settings: rejected change: %v", err)
	}
}

// applyConfig persists cfg and brings the monitor and tray menu in line. The
// caller must hold applyMu.
func applyConfig(cfg Config) error {
	saved, err := persistConfig(cfg)
	if err != nil {
		return err
	}
	configMu.Lock()
	current = saved
	configMu.Unlock()
	applyRuntime(saved)
	return nil
}

// persistConfig validates cfg, applies the autostart toggle, and writes the
// config file. It returns the config as saved, with LaunchAtLogin reflecting
// the real autostart state. Shared by the tray and the settings window, so it
// touches no in-process state.
func persistConfig(cfg Config) (Config, error) {
	if err := validateConfig(cfg); err != nil {
		return cfg, err
	}
	if cfg.Whitelist == nil {
		cfg.Whitelist = []string{}
	}

	// Apply the autostart toggle first and record what actually happened, so a
	// failure leaves the toggle reflecting the real state.
	if cfg.LaunchAtLogin != launchAtLoginEnabled() {
		if err := setLaunchAtLogin(cfg.LaunchAtLogin); err != nil {
			logf("settings: launch-at-login toggle failed: %v", err)
		} else {
			logf("settings: launch at login %v", cfg.LaunchAtLogin)
		}
		cfg.LaunchAtLogin = launchAtLoginEnabled()
	}

	if err := saveConfig(cfg); err != nil {
		logf("settings: save config error: %v", err)
		return cfg, fmt.Errorf("save error")
	}
	logf("settings: config saved (enabled=%v, threshold=%.0f%%, kill_after=%.0fs)",
		cfg.Enabled, cfg.CPUThreshold, cfg.KillDuration)
	return cfg, nil
}

// applyRuntime starts/stops the monitor to match cfg.Enabled and refreshes the
// tray. Without an active subscription the monitor never runs, whatever the
// config says. Both monitor calls are idempotent.
func applyRuntime(cfg Config) {
	if licensed() && cfg.Enabled {
		startMonitor()
	} else {
		stopMonitor()
	}
	setTrayStatus(baseStatus(cfg), false)
	syncMenu()
}

// baseStatus is the resting tray status label for cfg.
func baseStatus(cfg Config) string {
	switch {
	case !licensed():
		return licenseStatusLabel()
	case cfg.Enabled:
		return "Watching"
	default:
		return "Disabled"
	}
}

// syncMenu updates the menu to reflect the config and subscription state.
// nil-guarded so it is safe to call before the tray menu is built (e.g. from
// tests) without panicking.
func syncMenu() {
	if mToggle == nil {
		return
	}
	cfg := getConfig()

	// Locked: offer Subscribe / Refresh and grey out monitoring. Subscribed (in
	// a build that enforces licensing): offer Manage Subscription.
	ok := licensed()
	setVisible(mSubscribe, !ok)
	setVisible(mRefresh, !ok)
	setVisible(mManage, ok && licenseEnforced())
	if ok {
		mToggle.Enable()
	} else {
		mToggle.Disable()
	}
	if cfg.Enabled {
		mToggle.Check()
	} else {
		mToggle.Uncheck()
	}
}

func setVisible(item *systray.MenuItem, on bool) {
	if on {
		item.Show()
	} else {
		item.Hide()
	}
}

// openConfigFile opens config.json in the user's default editor. It is the
// fallback when the settings window can't be shown (no WebKitGTK).
func openConfigFile() {
	path, err := configPath()
	if err != nil {
		logf("settings: config path unavailable: %v", err)
		return
	}
	if _, err := os.Stat(path); err != nil {
		_ = saveConfig(getConfig())
	}
	openURL(path)
}

// notifyKilled is called from monitor.go after a process is terminated.
// It shows a desktop notification and updates the tray tooltip and status label.
func notifyKilled(name string, pid int, cpu float64) {
	// Desktop notification (non-blocking).
	notifyKilledToast(name, pid, cpu)

	if mStatus == nil {
		return
	}
	setTrayStatus(fmt.Sprintf("Killed: %s", name), true)
	systray.SetTooltip(fmt.Sprintf("Hotfix — Killed %s (PID %d, %.0f%% CPU)", name, pid, cpu))
	restoreTrayStatusAfter(3 * time.Second)
}

// flashTrayStatus shows a transient status label, then returns to normal.
func flashTrayStatus(label string) {
	setTrayStatus(label, false)
	restoreTrayStatusAfter(3 * time.Second)
}

// restoreTrayStatusAfter resets the status label and tooltip after d,
// replacing any pending reset.
func restoreTrayStatusAfter(d time.Duration) {
	trayMu.Lock()
	defer trayMu.Unlock()
	if statusTimeout != nil {
		statusTimeout.Stop()
	}
	statusTimeout = time.AfterFunc(d, func() {
		if mStatus == nil {
			return
		}
		setTrayStatus(baseStatus(getConfig()), false)
		systray.SetTooltip(defaultTooltip)
	})
}

// setTrayStatus updates the disabled status label at the top of the menu.
// nil-guarded so it is safe to call before the tray menu is built.
func setTrayStatus(label string, alert bool) {
	if mStatus == nil {
		return
	}
	prefix := "Hotfix — "
	if alert {
		prefix = "🔥 Hotfix — "
	}
	mStatus.SetTitle(prefix + label)
}
