//go:build windows

package main

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/getlantern/systray"
)

// appUserModelID identifies Hotfix to the Windows notification platform. It must
// match the AppUserModelID stamped on the Start Menu shortcut by the installer
// (see installer/hotfix.iss) and the ID used to create the toast notifier
// (see notify.go). Without this identity, Windows silently drops toast banners.
const appUserModelID = "BuildCraftLabs.Hotfix"

// setAppUserModelID gives this process an explicit AppUserModelID so Windows
// attributes toast notifications to Hotfix and shows them on screen instead of
// dropping them. Best-effort: any failure is logged and ignored.
func setAppUserModelID(id string) {
	proc := syscall.NewLazyDLL("shell32.dll").NewProc("SetCurrentProcessExplicitAppUserModelID")
	if err := proc.Find(); err != nil {
		logf("notify: SetCurrentProcessExplicitAppUserModelID unavailable: %v", err)
		return
	}
	p, err := syscall.UTF16PtrFromString(id)
	if err != nil {
		return
	}
	if ret, _, _ := proc.Call(uintptr(unsafe.Pointer(p))); ret != 0 {
		logf("notify: SetCurrentProcessExplicitAppUserModelID returned 0x%x", ret)
	}
}

// Tray icons: the color flame emoji (U+1F525), matching the flame next to
// "Hotfix Settings" on the settings page. Regenerate via gen-icons.ps1.
//
//go:embed assets/flame16.png
var flamePNG16 []byte

//go:embed assets/flame32.png
var flamePNG32 []byte

// Embed the app icon + version metadata into the .exe. Generates resource.syso
// from versioninfo.json (which points at assets/Hotfix.ico); `go build` then
// links it automatically. Run `go generate ./...` after editing versioninfo.json.
//
//go:generate go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest -64 -o resource.syso versioninfo.json

// --- Tray state ---
var (
	mStatus       *systray.MenuItem
	mToggle       *systray.MenuItem
	trayMu        sync.Mutex
	statusTimeout *time.Timer
)

func main() {
	// Set up logging before anything else.
	initLog()
	logf("Hotfix starting (version %s)", currentVersion)

	// Claim our notification identity so Windows shows toasts on screen.
	setAppUserModelID(appUserModelID)

	// If a previous run left a crash behind, surface it for one-click reporting.
	reportPendingCrash()

	// Load config.
	configMu.Lock()
	current = loadConfig()
	configMu.Unlock()

	// Hand control to systray — onReady and onExit run on its goroutine.
	systray.Run(onReady, onExit)
}

func onReady() {
	// Set tray icon and tooltip.
	systray.SetIcon(iconBytes())
	systray.SetTooltip("Hotfix — by BuildCraft Labs")

	// Build menu.
	mStatus = systray.AddMenuItem("Hotfix — Watching", "")
	mStatus.Disable()
	systray.AddSeparator()

	mToggle = systray.AddMenuItem("Enable Monitoring", "")
	syncToggleCheck()
	systray.AddSeparator()

	mSettings := systray.AddMenuItem("Settings...", "Open Hotfix settings")
	mUpdate := systray.AddMenuItem("Check for Updates", "Check GitHub for a newer release")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit Hotfix")

	// Start monitor if enabled.
	if getConfig().Enabled {
		startMonitor()
	}

	// Watch for system sleep events (KillOnSleep support).
	watchSleep()

	// Begin silent background auto-updates (launch check + periodic poll).
	startAutoUpdater()

	// Event loop. A panic while handling a click is captured and reported
	// rather than silently killing the tray (panics go to a discarded stderr
	// under -H windowsgui). Each action is wrapped so one bad handler can't
	// take down the loop.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				recordCrash("event-loop", r)
				reportPendingCrash()
			}
		}()
		for {
			select {
			case <-mToggle.ClickedCh:
				safe("toggle", toggleEnabled)

			case <-mSettings.ClickedCh:
				safeGo("settings", openSettings)

			case <-mUpdate.ClickedCh:
				safeGo("update", func() { checkForUpdates(false) })

			case <-mQuit.ClickedCh:
				systray.Quit()
				return

			case <-systrayQuitCh:
				systray.Quit()
				return
			}
		}
	}()
}

func onExit() {
	stopMonitor()
	terminateSettingsWindow()
	logf("Hotfix exiting")
	closeLog()
}

// toggleEnabled flips the Enabled flag in config and starts/stops monitoring.
func toggleEnabled() {
	cfg := getConfig()
	cfg.Enabled = !cfg.Enabled
	if err := setConfig(cfg); err != nil {
		logf("main: save config error: %v", err)
	}

	if cfg.Enabled {
		startMonitor()
		setTrayStatus("Watching", false)
	} else {
		stopMonitor()
		setTrayStatus("Disabled", false)
	}

	syncToggleCheck()
}

// syncToggleCheck updates the checkmark on the toggle menu item to reflect config.
// nil-guarded so it is safe to call before the tray menu is built (e.g. from tests
// or the settings window) without panicking.
func syncToggleCheck() {
	if mToggle == nil {
		return
	}
	if getConfig().Enabled {
		mToggle.Check()
	} else {
		mToggle.Uncheck()
	}
}

// notifyKilled is called from monitor.go after a successful taskkill.
// It shows a desktop toast and updates the tray tooltip and status label.
func notifyKilled(name string, pid int, cpu float64) {
	// Desktop toast notification (non-blocking).
	notifyKilledToast(name, pid, cpu)

	msg := fmt.Sprintf("Killed: %s", name)
	tooltip := fmt.Sprintf("Hotfix — Killed %s (PID %d, %.0f%% CPU)", name, pid, cpu)

	setTrayStatus(msg, true)
	systray.SetTooltip(tooltip)

	trayMu.Lock()
	if statusTimeout != nil {
		statusTimeout.Stop()
	}
	statusTimeout = time.AfterFunc(3*time.Second, func() {
		setTrayStatus("Watching", false)
		systray.SetTooltip("Hotfix — by BuildCraft Labs")
	})
	trayMu.Unlock()
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

// --- Tray icon ---

// iconBytes returns an ICO containing the 16x16 and 32x32 color flame PNGs
// (the U+1F525 emoji), matching the settings-page flame.
func iconBytes() []byte {
	return pngToICO(
		pngFrame{size: 16, data: flamePNG16},
		pngFrame{size: 32, data: flamePNG32},
	)
}

type pngFrame struct {
	size int
	data []byte
}

// pngToICO wraps one or more PNG images into a valid ICO container.
func pngToICO(frames ...pngFrame) []byte {
	var b bytes.Buffer
	le := binary.LittleEndian

	n := uint16(len(frames))
	// ICO header: reserved=0, type=1 (ICO), count=n
	binary.Write(&b, le, uint16(0))
	binary.Write(&b, le, uint16(1))
	binary.Write(&b, le, n)

	// Each ICONDIRENTRY is 16 bytes; image data follows all entries.
	offset := uint32(6 + 16*int(n))
	for _, f := range frames {
		sz := byte(f.size)                        // 0 means 256 for the ICO spec; 32→32
		b.WriteByte(sz)                           // width
		b.WriteByte(sz)                           // height
		b.WriteByte(0)                            // colorCount (0 = no palette)
		b.WriteByte(0)                            // reserved
		binary.Write(&b, le, uint16(1))           // planes
		binary.Write(&b, le, uint16(32))          // bitCount
		binary.Write(&b, le, uint32(len(f.data))) // imageSize
		binary.Write(&b, le, offset)              // offset
		offset += uint32(len(f.data))
	}

	for _, f := range frames {
		b.Write(f.data)
	}

	return b.Bytes()
}

// --- Logging ---

// maxLogBytes caps the log file size; when exceeded it is rolled to hotfix.log.1
// (one backup) and a fresh log is started, so the file never grows unbounded.
const maxLogBytes = 5 * 1024 * 1024

var (
	logFile  *os.File
	logMu    sync.Mutex
	logBytes int64 // approximate current size of logFile, guarded by logMu
)

// logFilePath returns the absolute path to the log file, or "" if it cannot be
// determined. Used by both initLog and the settings server's /log endpoint.
func logFilePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "Hotfix", "hotfix.log")
}

func initLog() {
	path := logFilePath()
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	logFile = f
	if fi, err := f.Stat(); err == nil {
		logBytes = fi.Size()
	}
}

func logf(format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	msg := fmt.Sprintf("[%s] "+format+"\n",
		append([]any{time.Now().Format("2006-01-02 15:04:05")}, args...)...)
	if logFile != nil {
		n, _ := logFile.WriteString(msg)
		logBytes += int64(n)
		if logBytes > maxLogBytes {
			rotateLogLocked()
		}
	}
	// In debug builds you can uncomment the line below:
	// os.Stderr.WriteString(msg)
}

// rotateLogLocked rolls hotfix.log → hotfix.log.1 (one backup) and reopens a
// fresh log. The caller must hold logMu.
func rotateLogLocked() {
	path := logFilePath()
	if path == "" || logFile == nil {
		return
	}
	_ = logFile.Close()
	backup := path + ".1"
	_ = os.Remove(backup)
	_ = os.Rename(path, backup)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		logFile = nil
		return
	}
	logFile = f
	logBytes = 0
}

func closeLog() {
	logMu.Lock()
	defer logMu.Unlock()
	if logFile != nil {
		_ = logFile.Close()
	}
}
