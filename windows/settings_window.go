//go:build windows

package main

import (
	_ "embed"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

// The settings UI is the embedded HTML loaded directly into a WebView2 popover
// (no HTTP server, no port). The page calls the hotfix* functions bound below
// via WebView2's native JS↔Go bridge instead of fetch().
//
//go:embed assets/settings.html
var settingsHTML string

// webView2DownloadURL is opened if the WebView2 runtime is missing (it ships
// with Windows 11, so this is a rare fallback).
const webView2DownloadURL = "https://developer.microsoft.com/microsoft-edge/webview2/"

// Popover size in device-independent-ish pixels (the settings content is 640px
// wide). Clamped to the work area at show time.
const (
	popWidth  = 700
	popHeight = 820
	popMargin = 12
)

var (
	settingsMu      sync.Mutex
	settingsView    webview2.WebView
	settingsHWND    uintptr
	settingsStarted bool // creation goroutine launched
	settingsFailed  bool // WebView2 unavailable; stop retrying
	settingsVisible atomic.Bool
	settingsShownAt atomic.Int64 // UnixNano of last show, for a dismiss grace period
	dismissOnce     sync.Once
)

// openSettings shows the settings popover, creating it on first use. Safe to
// call from the tray event loop; never blocks.
func openSettings() {
	settingsMu.Lock()
	switch {
	case settingsFailed:
		settingsMu.Unlock()
		settingsUnavailable()
		return
	case settingsView != nil:
		w := settingsView
		settingsMu.Unlock()
		w.Dispatch(showSettingsPopover)
		return
	case settingsStarted:
		settingsMu.Unlock() // still being created; it shows itself when ready
		return
	default:
		settingsStarted = true
		settingsMu.Unlock()
		safeGo("settings-window", runSettingsWindow)
	}
}

// runSettingsWindow owns the WebView2 window: it must create, drive, and tear
// it down on a single locked OS thread (Win32 message-loop requirement).
func runSettingsWindow() {
	runtime.LockOSThread()

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "Hotfix Settings",
			Width:  popWidth,
			Height: popHeight,
		},
	})
	if w == nil {
		logf("settings: WebView2 unavailable; cannot open settings window")
		settingsMu.Lock()
		settingsFailed = true
		settingsStarted = false
		settingsMu.Unlock()
		settingsUnavailable()
		return
	}

	hwnd := uintptr(w.Window())
	bindSettingsAPI(w)

	settingsMu.Lock()
	settingsView = w
	settingsHWND = hwnd
	settingsMu.Unlock()

	stylePopover(hwnd)
	w.SetHtml(settingsHTML)
	showSettingsPopover()
	startDismissWatcher()

	w.Run() // blocks until the window is destroyed
	w.Destroy()

	settingsMu.Lock()
	settingsView = nil
	settingsHWND = 0
	settingsStarted = false
	settingsVisible.Store(false)
	settingsMu.Unlock()
}

// terminateSettingsWindow stops the settings message loop on shutdown. Safe to
// call from any goroutine (Terminate is documented as thread-safe).
func terminateSettingsWindow() {
	settingsMu.Lock()
	w := settingsView
	settingsMu.Unlock()
	if w != nil {
		w.Terminate()
	}
}

// --- Native JS↔Go bridge (replaces the old HTTP endpoints) ---

func bindSettingsAPI(w webview2.WebView) {
	// Returned values are JSON-marshaled to JS; a non-nil error rejects the JS
	// promise with the error text (shown inline in the settings page).
	_ = w.Bind("hotfixGetConfig", func() (Config, error) { return getConfig(), nil })
	_ = w.Bind("hotfixSaveConfig", func(cfg Config) error { return applySavedConfig(cfg) })
	_ = w.Bind("hotfixGetLog", func() (string, error) { return readLogText(), nil })
	_ = w.Bind("hotfixCheckUpdates", func() error { safeGo("update", func() { checkForUpdates(false) }); return nil })
	_ = w.Bind("hotfixOpenLog", func() error { return openLogExternally() })
}

// applySavedConfig validates cfg, persists it, and starts/stops the monitor to
// match the Enabled flag. Shared by the settings binding and its tests.
func applySavedConfig(cfg Config) error {
	if cfg.CPUThreshold < 1 || cfg.CPUThreshold > 100 {
		return fmt.Errorf("cpu_threshold must be 1–100")
	}
	if cfg.KillDuration < 5 {
		return fmt.Errorf("kill_duration must be >= 5")
	}
	if cfg.Whitelist == nil {
		cfg.Whitelist = []string{}
	}
	if err := setConfig(cfg); err != nil {
		logf("settings: save config error: %v", err)
		return fmt.Errorf("save error")
	}
	logf("settings: config saved (enabled=%v, threshold=%.0f%%, kill_after=%.0fs)",
		cfg.Enabled, cfg.CPUThreshold, cfg.KillDuration)

	// Apply the autostart toggle to the registry. Non-fatal: the config is
	// already persisted, and loadConfig re-reads the real registry state next
	// launch, so a failure here self-corrects rather than wedging the UI.
	if err := setLaunchAtLogin(cfg.LaunchAtLogin); err != nil {
		logf("settings: launch-at-login toggle failed: %v", err)
	} else {
		logf("settings: launch at login %v", cfg.LaunchAtLogin)
	}

	// The monitor goroutine is only launched at startup when enabled, so toggling
	// Enabled here must start/stop it too. Both calls are idempotent.
	if cfg.Enabled {
		startMonitor()
		setTrayStatus("Watching", false)
	} else {
		stopMonitor()
		setTrayStatus("Disabled", false)
	}

	// Keep the tray menu's "Enable Monitoring" checkmark in sync with the state
	// chosen in the settings window. Without this, disabling monitoring in
	// Settings leaves the tray checkmark ticked (out of sync).
	syncToggleCheck()
	return nil
}

// readLogText returns the log file contents, or a friendly placeholder.
func readLogText() string {
	path := logFilePath()
	if path == "" {
		return "Log file location unavailable."
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return "No log entries yet."
	}
	return string(data)
}

// openLogExternally opens the log file in the user's default text viewer.
func openLogExternally() error {
	path := logFilePath()
	if path == "" {
		return fmt.Errorf("log file location unavailable")
	}
	// `start "" <path>` launches the default handler; the hidden cmd just avoids
	// a console flash (the editor itself is visible).
	return hiddenCmd("cmd", "/c", "start", "", path).Start()
}

// settingsUnavailable notifies the user when WebView2 is missing and points
// them at the runtime download (it ships with Windows 11, so this is rare).
func settingsUnavailable() {
	notifyToast("Hotfix — Settings unavailable",
		"The WebView2 Runtime is required to open Settings. Opening the download page…")
	openURL(webView2DownloadURL)
}

// --- Win32 popover plumbing ---

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procShowWindow               = user32.NewProc("ShowWindow")
	procSetWindowPos             = user32.NewProc("SetWindowPos")
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procGetWindowLongPtr         = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtr         = user32.NewProc("SetWindowLongPtrW")
	procSystemParametersInfo     = user32.NewProc("SystemParametersInfoW")

	// GWL_STYLE / GWL_EXSTYLE are negative; kept as vars so the uintptr
	// conversion is non-constant (a constant negative→uintptr won't compile).
	gwlStyle   = int32(-16)
	gwlExStyle = int32(-20)
)

const (
	wsPopup        = 0x80000000
	wsExToolWindow = 0x00000080
	wsExTopmost    = 0x00000008

	swHide = 0
	swShow = 5

	swpFrameChanged = 0x0020
	swpShowWindow   = 0x0040

	spiGetWorkArea = 0x0030
)

var hwndTopmost = ^uintptr(0) // HWND_TOPMOST = (HWND)-1

type winRect struct{ left, top, right, bottom int32 }

// stylePopover strips the window chrome and makes it a top-most tool window
// (no taskbar/alt-tab entry) — a popover, not a regular window.
func stylePopover(hwnd uintptr) {
	procSetWindowLongPtr.Call(hwnd, uintptr(gwlStyle), uintptr(uint32(wsPopup)))
	ex, _, _ := procGetWindowLongPtr.Call(hwnd, uintptr(gwlExStyle))
	procSetWindowLongPtr.Call(hwnd, uintptr(gwlExStyle), ex|wsExToolWindow|wsExTopmost)
}

// showSettingsPopover positions the window at the work-area's bottom-right
// corner (by the tray) and brings it to the front. Must run on the UI thread.
func showSettingsPopover() {
	settingsMu.Lock()
	hwnd := settingsHWND
	settingsMu.Unlock()
	if hwnd == 0 {
		return
	}

	var wa winRect
	procSystemParametersInfo.Call(uintptr(spiGetWorkArea), 0, uintptr(unsafe.Pointer(&wa)), 0)

	w, h := int32(popWidth), int32(popHeight)
	if maxW := wa.right - wa.left - 2*popMargin; w > maxW {
		w = maxW
	}
	if maxH := wa.bottom - wa.top - 2*popMargin; h > maxH {
		h = maxH
	}
	x := wa.right - w - popMargin
	y := wa.bottom - h - popMargin

	procSetWindowPos.Call(hwnd, hwndTopmost,
		uintptr(uint32(x)), uintptr(uint32(y)), uintptr(uint32(w)), uintptr(uint32(h)),
		uintptr(swpShowWindow|swpFrameChanged))
	procShowWindow.Call(hwnd, swShow)
	procSetForegroundWindow.Call(hwnd)

	settingsShownAt.Store(time.Now().UnixNano())
	settingsVisible.Store(true)
}

// foregroundPID returns the PID that owns the current foreground window, or 0 if
// it cannot be determined. Used to spare the app the user is actively using from
// being killed. Safe to call from the monitor goroutine.
func foregroundPID() int {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return 0
	}
	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return int(pid)
}

// hideSettingsPopover hides (does not destroy) the window. Must run on the UI thread.
func hideSettingsPopover() {
	settingsMu.Lock()
	hwnd := settingsHWND
	settingsMu.Unlock()
	settingsVisible.Store(false)
	if hwnd != 0 {
		procShowWindow.Call(hwnd, swHide)
	}
}

// startDismissWatcher hides the popover when it loses focus (click-away), the
// way a tray popover should behave. Polling the foreground window avoids
// subclassing WebView2's own window procedure.
func startDismissWatcher() {
	dismissOnce.Do(func() {
		safeGo("settings-dismiss", func() {
			t := time.NewTicker(200 * time.Millisecond)
			defer t.Stop()
			for range t.C {
				if !settingsVisible.Load() {
					continue
				}
				// Grace period so the window we just activated isn't dismissed
				// before the activation takes effect.
				if time.Since(time.Unix(0, settingsShownAt.Load())) < 500*time.Millisecond {
					continue
				}
				settingsMu.Lock()
				w, hwnd := settingsView, settingsHWND
				settingsMu.Unlock()
				if w == nil || hwnd == 0 {
					continue
				}
				if fg, _, _ := procGetForegroundWindow.Call(); fg != hwnd {
					w.Dispatch(hideSettingsPopover)
				}
			}
		})
	})
}
