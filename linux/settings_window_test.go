//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPopoverPlacement(t *testing.T) {
	screen := gdkRectangle{X: 0, Y: 0, Width: 1920, Height: 1080}
	cases := []struct {
		name       string
		wa         gdkRectangle
		x, y, w, h int
	}{
		// Bottom panel (Windows-like): bottom-right corner.
		{"bottom panel", gdkRectangle{0, 0, 1920, 1040}, 1920 - 700 - 12, 1040 - 820 - 12, 700, 820},
		// Top panel (GNOME): top-right corner, below the panel.
		{"top panel", gdkRectangle{0, 32, 1920, 1048}, 1920 - 700 - 12, 32 + 12, 700, 820},
		// Small screen: clamped to the work area minus margins.
		{"small screen", gdkRectangle{0, 0, 1366, 728}, 1366 - 700 - 12, 12, 700, 728 - 24},
	}
	for _, c := range cases {
		x, y, w, h := popoverPlacement(c.wa, screen)
		if x != c.x || y != c.y || w != c.w || h != c.h {
			t.Errorf("%s: got (%d,%d %dx%d), want (%d,%d %dx%d)", c.name, x, y, w, h, c.x, c.y, c.w, c.h)
		}
	}
}

// linuxSettingsEdits are the only intended differences between the Linux and
// Windows settings pages (besides the header comment).
var linuxSettingsEdits = [][2]string{
	{`<span class="header-badge">Windows</span>`, `<span class="header-badge">Linux</span>`},
	{`<small>Start Hotfix automatically when Windows starts</small>`, `<small>Start Hotfix automatically when you log in</small>`},
	{`<span class="about-item-value">` + currentVersion + `</span>`, `<span class="about-item-value">{{VERSION}}</span>`},
	{`<span class="about-item-value">Windows</span>`, `<span class="about-item-value">Linux</span>`},
	{`bound by the Go host (WebView2)`, `bound by the Go host (WebKitGTK)`},
}

// The Linux page must stay a copy of the Windows one: apply the intended edits
// to the Windows page and compare. (Also catches a version bump made in only
// one place: the Windows About card carries the literal version.)
func TestSettingsHTML_MatchesWindowsPage(t *testing.T) {
	win, err := os.ReadFile(filepath.Join("..", "windows", "assets", "settings.html"))
	if err != nil {
		t.Skipf("Windows page not available: %v", err)
	}
	want := strings.ReplaceAll(string(win), "\r\n", "\n")
	for _, e := range linuxSettingsEdits {
		if strings.Count(want, e[0]) != 1 {
			t.Fatalf("Windows page no longer contains exactly one %q", e[0])
		}
		want = strings.Replace(want, e[0], e[1], 1)
	}
	got := strings.ReplaceAll(settingsHTML, "\r\n", "\n")
	// Drop the Linux header comment (lines 2–4).
	lines := strings.SplitN(got, "\n", 5)
	if len(lines) == 5 && strings.HasPrefix(lines[1], "<!-- Linux copy") {
		got = lines[0] + "\n" + lines[4]
	}
	if got != want {
		t.Error("linux/assets/settings.html has drifted from windows/assets/settings.html; " +
			"copy the Windows page and re-apply only the platform edits")
	}
}

func TestRenderSettingsHTML(t *testing.T) {
	html := renderSettingsHTML()
	if strings.Contains(html, "{{VERSION}}") {
		t.Error("version placeholder not filled in")
	}
	if !strings.Contains(html, `<span class="about-item-value">`+currentVersion+`</span>`) {
		t.Error("About card should show the current version")
	}
	if !strings.Contains(html, `<span class="header-badge">Linux</span>`) || strings.Contains(html, ">Windows<") {
		t.Error("page should be labelled Linux, not Windows")
	}
	for _, fn := range []string{"hotfixGetConfig", "hotfixSaveConfig", "hotfixGetLog", "hotfixCheckUpdates", "hotfixOpenLog", "hotfixListApps"} {
		if !strings.Contains(html, fn+"(") {
			t.Errorf("page no longer calls %s", fn)
		}
	}
}

func TestSettingsSave(t *testing.T) {
	isolateXDG(t)
	t.Setenv(parentPIDEnv, "") // no tray to signal

	cfg := defaultConfig()
	cfg.CPUThreshold = 72
	cfg.KillDuration = 30
	cfg.Whitelist = []string{"blender"}
	arg, _ := json.Marshal(cfg)

	if _, err := settingsSave([]json.RawMessage{arg}); err != nil {
		t.Fatalf("valid save rejected: %v", err)
	}
	saved, err := readConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if saved.CPUThreshold != 72 || saved.KillDuration != 30 || len(saved.Whitelist) != 1 {
		t.Errorf("not saved: %+v", saved)
	}

	cfg.CPUThreshold = 500
	bad, _ := json.Marshal(cfg)
	if _, err := settingsSave([]json.RawMessage{bad}); err == nil {
		t.Error("out-of-range threshold must be rejected")
	}
	if saved, _ := readConfigFile(); saved.CPUThreshold != 72 {
		t.Error("a rejected save must not change the file")
	}
	if _, err := settingsSave(nil); err == nil {
		t.Error("missing argument must be rejected")
	}
	if _, err := settingsSave([]json.RawMessage{json.RawMessage(`"nope"`)}); err == nil {
		t.Error("malformed argument must be rejected")
	}
}

// Only the parent that started the window may be signalled. (The success path
// isn't exercised: it would signal the test runner.)
func TestSignalTray_RejectsWrongParent(t *testing.T) {
	for _, v := range []string{"", "garbage", "1", strconv.Itoa(os.Getpid())} {
		t.Setenv(parentPIDEnv, v)
		if err := signalTray(0); err == nil {
			t.Errorf("%s=%q must not be signalled", parentPIDEnv, v)
		}
	}
}

func TestReadLogText(t *testing.T) {
	isolateXDG(t)
	if got := readLogText(); got != "No log entries yet." {
		t.Errorf("missing log: %q", got)
	}

	path := logFilePath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	var b bytes.Buffer
	for i := 0; b.Len() < maxLogView+50_000; i++ {
		b.WriteString("[2026-01-01 00:00:00] line " + strconv.Itoa(i) + "\n")
	}
	b.WriteString("[2026-01-01 00:00:00] LAST\n")
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readLogText()
	if len(got) > maxLogView {
		t.Errorf("read %d bytes, want at most %d", len(got), maxLogView)
	}
	if !strings.HasPrefix(got, "[2026") {
		t.Errorf("should start on a whole line, got %q", got[:30])
	}
	if !strings.HasSuffix(got, "LAST\n") {
		t.Error("newest entry missing")
	}
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{max: 10}
	tb.Write([]byte("hello "))
	tb.Write([]byte("wonderful world"))
	if got := tb.String(); got != "rful world" {
		t.Errorf("got %q", got)
	}
}

func TestEnsurePrivateDir(t *testing.T) {
	base := t.TempDir()
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o777); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(open, 0o777) // undo umask
	if err := ensurePrivateDir(open); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(open); fi.Mode().Perm() != 0o700 {
		t.Errorf("group/world-writable dir should be tightened to 0700, got %v", fi.Mode().Perm())
	}

	link := filepath.Join(base, "link")
	if err := os.Symlink(open, link); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(link); err == nil {
		t.Error("a symlink must be refused")
	}
}

func TestExtractWebviewLib(t *testing.T) {
	isolateXDG(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	lib, embedErr := webviewLibFS.ReadFile("webviewlib/" + webviewLibName)
	path, err := extractWebviewLib()
	if embedErr != nil {
		// Local/dev build: no library embedded.
		if !errors.Is(err, errNoWebviewLib) {
			t.Fatalf("without an embedded library, want errNoWebviewLib, got %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, os.Getenv("XDG_RUNTIME_DIR")) {
		t.Errorf("library should go to the private runtime dir, got %s", path)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, lib) {
		t.Fatal("extracted library differs from the embedded one")
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Errorf("library dir should be 0700, got %v", fi.Mode().Perm())
	}

	// A tampered file is replaced, not trusted.
	if err := os.WriteFile(path, []byte("not a library"), 0o700); err != nil {
		t.Fatal(err)
	}
	if again, err := extractWebviewLib(); err != nil || again != path {
		t.Fatalf("re-extract: %v %s", err, again)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, lib) {
		t.Error("tampered library was not replaced")
	}
}
