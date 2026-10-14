//go:build windows

package main

import (
	"encoding/binary"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// buildShortcut assembles a minimal .lnk: header, optional ID list, a
// LinkInfo carrying target = base+suffix — ANSI, or Unicode (header 0x24) —
// and, when args is set, StringData with a working dir and the arguments.
func buildShortcut(base, suffix string, unicode, withIDList bool, args ...string) []byte {
	le := binary.LittleEndian
	hdr := make([]byte, 0x4C)
	le.PutUint32(hdr[0:], 0x4C)
	flags := uint32(0x02) // HasLinkInfo
	if withIDList {
		flags |= 0x01
	}
	if len(args) > 0 {
		flags |= 0x10 | 0x20 | 0x80 // HasWorkingDir, HasArguments, IsUnicode
	}
	le.PutUint32(hdr[0x14:], flags)
	out := hdr
	if withIDList {
		idl := []byte{0x04, 0x00, 0xAA, 0xBB} // size 4: two junk bytes + terminator stand-in
		sz := make([]byte, 2)
		le.PutUint16(sz, uint16(len(idl)))
		out = append(out, sz...)
		out = append(out, idl...)
	}

	hdrSize := uint32(0x1C)
	if unicode {
		hdrSize = 0x24
	}
	volume := make([]byte, 0x10)
	le.PutUint32(volume[0:], 0x10)
	ansi := func(s string) []byte { return append([]byte(s), 0) }
	wide := func(s string) []byte {
		var b []byte
		for _, c := range utf16.Encode([]rune(s)) {
			b = le.AppendUint16(b, c)
		}
		return append(b, 0, 0)
	}

	body := append([]byte{}, volume...)
	volOff := hdrSize
	baseOff := volOff + uint32(len(volume))
	body = append(body, ansi(base)...)
	sufOff := hdrSize + uint32(len(body))
	body = append(body, ansi(suffix)...)
	var ubaseOff, usufOff uint32
	if unicode {
		ubaseOff = hdrSize + uint32(len(body))
		body = append(body, wide(base)...)
		usufOff = hdrSize + uint32(len(body))
		body = append(body, wide(suffix)...)
	}

	li := make([]byte, hdrSize)
	le.PutUint32(li[0:], hdrSize+uint32(len(body)))
	le.PutUint32(li[4:], hdrSize)
	le.PutUint32(li[8:], 0x01) // VolumeIDAndLocalBasePath
	le.PutUint32(li[0x0C:], volOff)
	le.PutUint32(li[0x10:], baseOff)
	le.PutUint32(li[0x18:], sufOff)
	if unicode {
		le.PutUint32(li[0x1C:], ubaseOff)
		le.PutUint32(li[0x20:], usufOff)
	}
	out = append(out, li...)
	out = append(out, body...)
	if len(args) > 0 {
		for _, s := range []string{`C:\Work`, args[0]} { // working dir, then arguments
			u := utf16.Encode([]rune(s))
			out = le.AppendUint16(out, uint16(len(u)))
			for _, c := range u {
				out = le.AppendUint16(out, c)
			}
		}
	}
	return out
}

func TestParseShortcut(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"ansi", buildShortcut(`C:\Program Files\Mozilla Firefox\firefox.exe`, "", false, false), `C:\Program Files\Mozilla Firefox\firefox.exe`},
		{"ansi with ID list", buildShortcut(`C:\Tools\app.exe`, "", false, true), `C:\Tools\app.exe`},
		{"base + suffix", buildShortcut(`C:\Program Files\`, `Blender\blender.exe`, false, false), `C:\Program Files\Blender\blender.exe`},
		{"unicode", buildShortcut(`C:\Programme\Grafik\Größe.exe`, "", true, true), `C:\Programme\Grafik\Größe.exe`},
	}
	for _, c := range cases {
		got, args, ok := parseShortcut(c.data)
		if !ok || got != c.want || args != "" {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, ok, c.want)
		}
	}

	bad := buildShortcut(`C:\x.exe`, "", false, false)
	for name, data := range map[string][]byte{
		"empty":       nil,
		"not a lnk":   []byte(strings.Repeat("x", 200)),
		"truncated":   bad[:0x4C+10],
		"no linkinfo": func() []byte { b := append([]byte{}, bad...); b[0x14] = 0; return b }(),
	} {
		if _, _, ok := parseShortcut(data); ok {
			t.Errorf("%s: must not parse", name)
		}
	}
}

func TestParseShortcut_Arguments(t *testing.T) {
	data := buildShortcut(`C:\Users\me\AppData\Local\Discord\Update.exe`, "", true, true, `--processStart "Discord.exe"`)
	target, args, ok := parseShortcut(data)
	if !ok || target != `C:\Users\me\AppData\Local\Discord\Update.exe` || args != `--processStart "Discord.exe"` {
		t.Fatalf("got %q %q %v", target, args, ok)
	}
}

func TestProcessStartArg(t *testing.T) {
	cases := map[string]string{
		`--processStart "Discord.exe"`:                        "Discord.exe",
		`--processStart slack.exe --process-start-args "--x"`: "slack.exe",
		`--processStart="Teams.exe"`:                          "Teams.exe",
		`--other`:                                             "",
		``:                                                    "",
	}
	for in, want := range cases {
		if got := processStartArg(in); got != want {
			t.Errorf("processStartArg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShortcutApp(t *testing.T) {
	cases := []struct {
		label, target string
		args          string
		want          installedApp
		ok            bool
	}{
		{"Discord", `C:\Users\me\AppData\Local\Discord\Update.exe`, `--processStart "Discord.exe"`, installedApp{"Discord", "Discord"}, true},
		{"Some Updater", `C:\Program Files\X\Update.exe`, ``, installedApp{"Update", "Some Updater"}, true},
		{"Firefox", `C:\Program Files\Mozilla Firefox\firefox.exe`, "", installedApp{"firefox", "Firefox"}, true},
		{"Visual Studio Code", `C:\Users\me\AppData\Local\Programs\Microsoft VS Code\Code.exe`, "", installedApp{"Code", "Visual Studio Code"}, true},
		{"Readme", `C:\Program Files\X\readme.txt`, "", installedApp{}, false},
		{"Uninstall Foo", `C:\Program Files\Foo\helper.exe`, "", installedApp{}, false},
		{"Foo", `C:\Program Files\Foo\unins000.exe`, "", installedApp{}, false},
		{"File Explorer", `C:\Windows\explorer.exe`, "", installedApp{}, false}, // protected
		{"Hotfix", `C:\Users\me\AppData\Local\Programs\Hotfix\Hotfix.exe`, "", installedApp{}, false},
	}
	for _, c := range cases {
		got, ok := shortcutApp(c.label, c.target, c.args)
		if ok != c.ok || got != c.want {
			t.Errorf("shortcutApp(%q, %q, %q) = %+v, %v; want %+v, %v", c.label, c.target, c.args, got, ok, c.want, c.ok)
		}
	}
}

func TestMergeApps(t *testing.T) {
	running := []runningApp{{Name: "chrome", CPU: 40}, {Name: "notepad", CPU: 1}}
	installed := []installedApp{
		{Name: "blender", Label: "Blender"},
		{Name: "Chrome", Label: "Google Chrome"}, // running: labels it, not listed again
		{Name: "gimp", Label: "GIMP"},
	}
	want := []appEntry{
		{Name: "chrome", Label: "Google Chrome", CPU: 40, Running: true},
		{Name: "notepad", CPU: 1, Running: true},
		{Name: "blender", Label: "Blender"},
		{Name: "gimp", Label: "GIMP"},
	}
	if got := mergeApps(running, installed); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

// The cache returns at once (empty on the very first call) and fills in the
// background.
func TestCachedInstalledApps(t *testing.T) {
	installedMu.Lock()
	installedCache, installedAt, installedBusy = nil, time.Time{}, false
	installedMu.Unlock()

	start := time.Now()
	_ = cachedInstalledApps()
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("cachedInstalledApps must not wait on the scan (took %v)", d)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		installedMu.Lock()
		done := !installedAt.IsZero()
		installedMu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background scan never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Scans this machine's real Start Menu.
func TestInstalledApps_Live(t *testing.T) {
	apps := installedApps()
	if len(apps) == 0 {
		t.Skip("no Start Menu shortcuts to programs on this machine")
	}
	seen := map[string]bool{}
	for _, a := range apps {
		low := strings.ToLower(a.Name)
		if strings.HasSuffix(low, ".exe") || a.Label == "" || seen[low] || isProtected(a.Name) {
			t.Errorf("bad entry %+v", a)
		}
		seen[low] = true
	}
	t.Logf("%d installed apps, e.g. %+v", len(apps), apps[:min(5, len(apps))])
}
