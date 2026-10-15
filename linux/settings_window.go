//go:build linux

package main

// The settings window is the Windows app's settings popover — the same page
// (assets/settings.html, a copy of windows/assets/settings.html) calling the
// same hotfix* functions — rendered by WebKitGTK through the webview library.
//
// It runs as its own process, `hotfix --settings`, started by the tray:
//   - GTK gets a fresh main thread each time the window opens, and the tray
//     process never loads GTK or WebKitGTK at all, so it keeps running on
//     systems without them;
//   - a crash, or a missing WebKitGTK, only takes the window down.
// The window saves straight to config.json and signals the tray process:
// SIGUSR2 = reload the config now, SIGUSR1 = check for updates.

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed assets/settings.html
var settingsHTML string

const (
	settingsFlag         = "--settings"
	settingsSelfTestFlag = "--settings-selftest" // used by CI; see runSettingsWindow
	exitNoWebview        = 3                     // the settings window can't be shown here
	parentPIDEnv         = "HOTFIX_PARENT_PID"
)

func init() {
	// GTK must run on the main thread: locking it in init guarantees that
	// main.main — and so the settings window — starts there.
	if len(os.Args) > 1 && (os.Args[1] == settingsFlag || os.Args[1] == settingsSelfTestFlag) {
		runtime.LockOSThread()
	}
}

// renderSettingsHTML fills in the version shown on the About card.
func renderSettingsHTML() string {
	return strings.ReplaceAll(settingsHTML, "{{VERSION}}", currentVersion)
}

// runSettingsWindow is the settings process; it returns the exit code. With
// selftest, CI opens the window under Xvfb, drives the page through its
// bindings, verifies the result, and exits instead of waiting for the user.
func runSettingsWindow(selftest bool) int {
	initLog()
	defer closeLog()

	path, err := extractWebviewLib()
	if err == nil {
		err = loadWebviewLib(path)
	}
	var w *webView
	if err == nil {
		w, err = newWebView(false)
	}
	if err != nil {
		logf("settings: window unavailable: %v", err)
		fmt.Fprintln(os.Stderr, "settings:", err)
		return exitNoWebview
	}
	defer w.Destroy()

	w.SetTitle("Hotfix Settings")
	win := w.Window()
	popover := false
	if err := loadGTK(); err != nil {
		logf("settings: popover styling unavailable: %v", err)
		w.SetSize(popWidth, popHeight)
	} else {
		makePopover(w, win)
		popover = true
	}

	bindSettingsAPI(w)
	var st *selfTest
	if selftest {
		st = startSelfTest(w)
	}

	w.SetHTML(renderSettingsHTML())
	shownAt := time.Now()
	if popover {
		gtk.present(win)
		// Click-away closes it, like the Windows popover — after a short grace
		// period so the window's own activation doesn't. (Not in the self-test:
		// Xvfb has no window manager to give it focus.)
		if !selftest {
			watchFocusOut(win, func() {
				if time.Since(shownAt) > 700*time.Millisecond {
					w.Terminate()
				}
			})
		}
	}

	// The tray sends SIGUSR1 when Settings… is clicked while the window is
	// already open: bring it to the front.
	raise := make(chan os.Signal, 1)
	signal.Notify(raise, syscall.SIGUSR1)
	go func() {
		for range raise {
			w.Dispatch(func() {
				shownAt = time.Now()
				if popover {
					gtk.present(win)
				}
			})
		}
	}()

	w.Run()
	if st != nil {
		return st.finish()
	}
	return 0
}

// bindSettingsAPI exposes the functions the settings page calls — the same
// names and shapes as the Windows app's WebView2 bindings.
func bindSettingsAPI(w *webView) {
	w.Bind("hotfixGetConfig", func([]json.RawMessage) (any, error) {
		return loadConfig(), nil
	})
	w.Bind("hotfixSaveConfig", settingsSave)
	w.Bind("hotfixGetLog", func([]json.RawMessage) (any, error) {
		return readLogText(), nil
	})
	w.Bind("hotfixCheckUpdates", func([]json.RawMessage) (any, error) {
		return nil, signalTray(syscall.SIGUSR1)
	})
	w.Bind("hotfixListApps", func([]json.RawMessage) (any, error) {
		return listApps()
	})
	w.Bind("hotfixOpenLog", func([]json.RawMessage) (any, error) {
		path := logFilePath()
		if path == "" {
			return nil, errors.New("log file location unavailable")
		}
		openURL(path)
		return nil, nil
	})
	// Linux extra: Esc closes the popover.
	w.Bind("hotfixClose", func([]json.RawMessage) (any, error) {
		w.Terminate()
		return nil, nil
	})
	w.Init(`document.addEventListener('keydown', function (e) { if (e.key === 'Escape') hotfixClose(); });`)
}

// settingsSave is hotfixSaveConfig: validate, apply the autostart toggle and
// save — then tell the tray to pick the change up. Validation errors reject
// the page's promise, which shows them inline.
func settingsSave(args []json.RawMessage) (any, error) {
	if len(args) != 1 {
		return nil, errors.New("expected one argument")
	}
	var cfg Config
	if err := json.Unmarshal(args[0], &cfg); err != nil {
		return nil, err
	}
	if _, err := persistConfig(cfg); err != nil {
		return nil, err
	}
	if err := signalTray(syscall.SIGUSR2); err != nil {
		logf("settings: %v (the tray picks the change up from the file)", err)
	}
	return nil, nil
}

// signalTray signals the tray process that opened this window. The parent PID
// comes from the environment and must still be our parent, so an orphaned
// window never signals an unrelated process.
func signalTray(sig syscall.Signal) error {
	ppid, err := strconv.Atoi(os.Getenv(parentPIDEnv))
	if err != nil || ppid <= 1 || ppid != os.Getppid() {
		return errors.New("Hotfix isn't running")
	}
	return syscall.Kill(ppid, sig)
}

// maxLogView caps how much of the log the page loads (it shows the last 200
// lines anyway), so a large log never stalls the window.
const maxLogView = 256 << 10

// readLogText returns the tail of the log file, or a friendly placeholder.
func readLogText() string {
	path := logFilePath()
	if path == "" {
		return "Log file location unavailable."
	}
	f, err := os.Open(path)
	if err != nil {
		return "No log entries yet."
	}
	defer f.Close()
	trimmed := false
	if fi, err := f.Stat(); err == nil && fi.Size() > maxLogView {
		if _, err := f.Seek(fi.Size()-maxLogView, io.SeekStart); err == nil {
			trimmed = true
		}
	}
	data, err := io.ReadAll(f)
	if err != nil || len(data) == 0 {
		return "No log entries yet."
	}
	text := string(data)
	if trimmed { // drop the partial first line
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	return text
}

// --- Tray side ---

var (
	settingsMu   sync.Mutex
	settingsProc *os.Process
)

// openSettings shows the settings window, starting its process if it isn't
// already open (an open one is brought to the front instead).
func openSettings() {
	settingsMu.Lock()
	defer settingsMu.Unlock()

	if settingsProc != nil {
		_ = settingsProc.Signal(syscall.SIGUSR1)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		logf("settings: %v", err)
		go settingsUnavailable()
		return
	}
	cmd := exec.Command(exe, settingsFlag)
	cmd.Env = append(os.Environ(), parentPIDEnv+"="+strconv.Itoa(os.Getpid()))
	stderr := &tailBuffer{max: 4096} // GTK/WebKit diagnostics, logged on failure
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		logf("settings: start window: %v", err)
		go settingsUnavailable()
		return
	}
	settingsProc = cmd.Process

	safeGo("settings-window", func() {
		err := cmd.Wait()
		settingsMu.Lock()
		settingsProc = nil
		settingsMu.Unlock()
		switch {
		case cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == exitNoWebview:
			logf("settings: window unavailable: %s", strings.TrimSpace(stderr.String()))
			settingsUnavailable()
		case err != nil:
			logf("settings: window exited: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
	})
}

// settingsUnavailable is the fallback when the window can't be shown (no
// WebKitGTK, or no display): explain, and open config.json instead.
func settingsUnavailable() {
	notifyToast("Hotfix — Settings window unavailable",
		"It needs WebKitGTK (libwebkit2gtk-4.1). Opening the settings file instead.")
	openConfigFile()
}

// closeSettingsWindow closes an open settings window (the tray is quitting).
func closeSettingsWindow() {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	if settingsProc != nil {
		_ = settingsProc.Signal(syscall.SIGTERM)
	}
}

// tailBuffer is an io.Writer that keeps only the last max bytes written.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// --- Self-test (CI) ---

// selfTestJS runs in the page once it has loaded its settings: it reads what
// the page shows, round-trips the bindings (load, save, a rejected save, log),
// and reports back through hotfixSelfTestReport.
const selfTestJS = `
window.addEventListener('load', function () {
  setTimeout(async function () {
    var r = {};
    try {
      r.title = (document.querySelector('h1') || {}).textContent || '';
      r.badge = (document.querySelector('.header-badge') || {}).textContent || '';
      r.about = Array.prototype.map.call(document.querySelectorAll('.about-item'),
        function (e) { return e.textContent.replace(/\s+/g, ' ').trim(); }).join(' | ');
      r.cpu = document.getElementById('cpu-threshold').value;
      var cfg = await hotfixGetConfig();
      r.configThreshold = cfg.cpu_threshold;
      cfg.cpu_threshold = 65;
      await hotfixSaveConfig(cfg);
      try {
        await hotfixSaveConfig(Object.assign({}, cfg, { cpu_threshold: 500 }));
        r.rejected = '';
      } catch (e) {
        r.rejected = String((e && e.message) || e);
      }
      r.log = typeof (await hotfixGetLog());
      var apps = await hotfixListApps();
      r.apps = Array.isArray(apps) ? apps.length : -1;
      // Open the exclusions dropdown the way a click does, and count its rows.
      document.getElementById('wl-input').dispatchEvent(new MouseEvent('click'));
      await new Promise(function (done) { setTimeout(done, 2000); });
      r.menuOptions = document.querySelectorAll('#wl-menu .wl-option').length;
      r.groups = Array.prototype.map.call(document.querySelectorAll('#wl-menu .wl-menu-head'),
        function (e) { return e.textContent; });
      r.labels = Array.prototype.map.call(document.querySelectorAll('#wl-menu .wl-option-label'),
        function (e) { return e.textContent; });
    } catch (e) {
      r.error = String((e && e.message) || e);
    }
    hotfixSelfTestReport(r);
  }, 2000);
});
`

type selfTestReport struct {
	Title           string   `json:"title"`
	Badge           string   `json:"badge"`
	About           string   `json:"about"`
	CPU             string   `json:"cpu"`
	ConfigThreshold float64  `json:"configThreshold"`
	Rejected        string   `json:"rejected"`
	Log             string   `json:"log"`
	Apps            int      `json:"apps"`
	MenuOptions     int      `json:"menuOptions"`
	Groups          []string `json:"groups"`
	Labels          []string `json:"labels"`
	Error           string   `json:"error"`
}

type selfTest struct {
	mu       sync.Mutex
	report   *selfTestReport
	timedOut bool
}

func startSelfTest(w *webView) *selfTest {
	st := &selfTest{}
	w.Bind("hotfixSelfTestReport", func(args []json.RawMessage) (any, error) {
		var r selfTestReport
		if len(args) == 1 {
			_ = json.Unmarshal(args[0], &r)
		}
		st.mu.Lock()
		st.report = &r
		st.mu.Unlock()
		w.Terminate()
		return nil, nil
	})
	w.Init(selfTestJS)
	time.AfterFunc(60*time.Second, func() {
		st.mu.Lock()
		st.timedOut = true
		st.mu.Unlock()
		w.Terminate()
	})
	return st
}

// finish checks the report and the saved config; 0 = pass.
func (st *selfTest) finish() int {
	st.mu.Lock()
	r, timedOut := st.report, st.timedOut
	st.mu.Unlock()
	if r == nil {
		fmt.Println("settings self-test: FAIL — no report from the page (timed out:", timedOut, ")")
		return 1
	}
	out, _ := json.MarshalIndent(r, "", "  ")
	fmt.Printf("settings self-test report:\n%s\n", out)

	saved, err := readConfigFile()
	var problems []string
	check := func(ok bool, msg string) {
		if !ok {
			problems = append(problems, msg)
		}
	}
	check(r.Error == "", "page error: "+r.Error)
	check(r.Title == "Hotfix Settings", "title")
	check(r.Badge == "Linux", "platform badge")
	check(strings.Contains(r.About, "Linux") && strings.Contains(r.About, currentVersion), "About card")
	check(r.CPU == strconv.Itoa(int(r.ConfigThreshold)), "CPU slider doesn't show the loaded threshold")
	check(r.Rejected != "", "an invalid save was not rejected")
	check(r.Log == "string", "hotfixGetLog")
	check(r.Apps > 0, "hotfixListApps returned no apps")
	check(r.MenuOptions > 0, "the exclusions dropdown showed no apps")
	// CI installs a fake app (a .desktop entry) and names it here, to check
	// the dropdown's "Installed" group end to end.
	if want := os.Getenv("HOTFIX_SELFTEST_EXPECT_APP"); want != "" {
		check(slices.Contains(r.Groups, "Installed") && slices.Contains(r.Labels, want),
			"installed app "+want+" missing from the dropdown")
	}
	check(err == nil && saved.CPUThreshold == 65, "hotfixSaveConfig didn't save the new threshold")
	if len(problems) > 0 {
		fmt.Println("settings self-test: FAIL —", strings.Join(problems, "; "))
		return 1
	}
	fmt.Println("settings self-test: PASS")
	return 0
}
