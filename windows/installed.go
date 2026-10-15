//go:build windows

package main

import (
	"encoding/binary"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// installedApp is an application installed on the system, for the exclusions
// dropdown: Name is what an exclusion matches (the program's image name
// without .exe), Label is what the Start Menu calls it.
type installedApp struct {
	Name  string
	Label string
}

// Limits so a huge Start Menu can't stall the settings window (the binding
// runs on the UI thread).
const (
	maxShortcutFiles = 3000
	maxShortcutBytes = 1 << 20
)

// startMenuDirs are the all-users and per-user Start Menu program folders.
func startMenuDirs() []string {
	var dirs []string
	if pd := os.Getenv("ProgramData"); pd != "" {
		dirs = append(dirs, filepath.Join(pd, `Microsoft\Windows\Start Menu\Programs`))
	}
	if ad := os.Getenv("APPDATA"); ad != "" {
		dirs = append(dirs, filepath.Join(ad, `Microsoft\Windows\Start Menu\Programs`))
	}
	return dirs
}

// The Start Menu scan can take a second or more (antivirus inspects every
// shortcut opened), and the binding runs on the UI thread, so the result is
// cached: scanned in the background at startup, refreshed when stale.
const installedTTL = 10 * time.Minute

var (
	installedMu    sync.Mutex
	installedCache []installedApp
	installedAt    time.Time
	installedBusy  bool
)

// cachedInstalledApps returns the latest scan without waiting, starting a
// background rescan when it is older than installedTTL (or missing).
func cachedInstalledApps() []installedApp {
	installedMu.Lock()
	defer installedMu.Unlock()
	if !installedBusy && time.Since(installedAt) > installedTTL {
		installedBusy = true
		safeGo("installed-apps", refreshInstalledApps)
	}
	return installedCache
}

func refreshInstalledApps() {
	apps := installedApps()
	installedMu.Lock()
	installedCache, installedAt, installedBusy = apps, time.Now(), false
	installedMu.Unlock()
}

// installedApps lists the programs with a Start Menu shortcut — what Windows
// users think of as their installed apps — sorted by label. Shortcuts to
// documents, web pages, uninstallers, protected processes and Hotfix itself
// are left out.
func installedApps() []installedApp {
	seen := map[string]bool{}
	var apps []installedApp
	files := 0
	for _, root := range startMenuDirs() {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable: skip, keep walking
			}
			if d.IsDir() || !strings.EqualFold(filepath.Ext(d.Name()), ".lnk") {
				return nil
			}
			if files++; files > maxShortcutFiles {
				return filepath.SkipAll
			}
			f, err := os.Open(path)
			if err != nil {
				return nil
			}
			data, err := io.ReadAll(io.LimitReader(f, maxShortcutBytes))
			f.Close()
			if err != nil {
				return nil
			}
			target, args, ok := parseShortcut(data)
			if !ok {
				return nil
			}
			label := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))
			app, ok := shortcutApp(label, target, args)
			if !ok || seen[strings.ToLower(app.Name)] {
				return nil
			}
			seen[strings.ToLower(app.Name)] = true
			apps = append(apps, app)
			return nil
		})
	}
	sort.SliceStable(apps, func(i, j int) bool {
		return strings.ToLower(apps[i].Label) < strings.ToLower(apps[j].Label)
	})
	return apps
}

// shortcutApp turns a shortcut's label, target and arguments into an app, or
// ok=false when the target isn't a program worth listing. Apps installed with
// Squirrel (Discord, Slack, ...) launch through "Update.exe --processStart
// App.exe"; the app named there is what actually runs.
func shortcutApp(label, target, args string) (installedApp, bool) {
	base := filepath.Base(strings.ReplaceAll(target, "/", `\`))
	if strings.EqualFold(base, "Update.exe") {
		if started := processStartArg(args); started != "" {
			base = filepath.Base(started)
		}
	}
	if !strings.EqualFold(filepath.Ext(base), ".exe") {
		return installedApp{}, false // documents, URLs, folders, scripts
	}
	name := base[:len(base)-len(".exe")]
	low := strings.ToLower(name)
	if name == "" || strings.HasPrefix(low, "unins") || strings.Contains(strings.ToLower(label), "uninstall") ||
		low == "hotfix" || isProtected(name) {
		return installedApp{}, false
	}
	return installedApp{Name: name, Label: label}, true
}

// processStartArg returns the X in "--processStart X" (X may be quoted).
func processStartArg(args string) string {
	i := strings.Index(strings.ToLower(args), "--processstart")
	if i < 0 {
		return ""
	}
	rest := strings.TrimLeft(args[i+len("--processStart"):], " =")
	if strings.HasPrefix(rest, `"`) {
		if j := strings.IndexByte(rest[1:], '"'); j >= 0 {
			return rest[1 : 1+j]
		}
		return ""
	}
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		return rest[:j]
	}
	return rest
}

// parseShortcut extracts the target path and command-line arguments from a
// Windows shortcut (.lnk, the [MS-SHLLINK] format). The target is the
// LinkInfo structure's local base path plus common path suffix, preferring
// the Unicode fields when present. ok is false for shortcuts without a local
// target (e.g. MSI "advertised" shortcuts).
func parseShortcut(data []byte) (target, args string, ok bool) {
	le := binary.LittleEndian
	const headerSize = 0x4C
	if len(data) < headerSize || le.Uint32(data[0:4]) != headerSize {
		return "", "", false
	}
	const (
		hasLinkTargetIDList = 0x01
		hasLinkInfo         = 0x02
	)
	flags := le.Uint32(data[0x14:])
	off := headerSize
	if flags&hasLinkTargetIDList != 0 {
		if off+2 > len(data) {
			return "", "", false
		}
		off += 2 + int(le.Uint16(data[off:]))
	}
	if flags&hasLinkInfo == 0 || off+0x1C > len(data) {
		return "", "", false
	}
	li := data[off:]
	size, hdrSize, liFlags := le.Uint32(li[0:]), le.Uint32(li[4:]), le.Uint32(li[8:])
	if size < 0x1C || int(size) > len(li) {
		return "", "", false
	}
	li = li[:size]
	const volumeIDAndLocalBasePath = 0x01
	if liFlags&volumeIDAndLocalBasePath == 0 {
		return "", "", false
	}

	if hdrSize >= 0x24 && len(li) >= 0x24 {
		if base := utf16At(li, le.Uint32(li[0x1C:])); base != "" {
			target = joinTarget(base, utf16At(li, le.Uint32(li[0x20:])))
		}
	}
	if target == "" {
		base := ansiAt(li, le.Uint32(li[0x10:]))
		if base == "" {
			return "", "", false
		}
		target = joinTarget(base, ansiAt(li, le.Uint32(li[0x18:])))
	}
	return target, shortcutArguments(data[off+int(size):], flags), true
}

// shortcutArguments reads COMMAND_LINE_ARGUMENTS from the StringData section
// that follows LinkInfo: counted strings, present per the header flags, in a
// fixed order; UTF-16 when the IsUnicode flag is set.
func shortcutArguments(sd []byte, flags uint32) string {
	const (
		hasName         = 0x04
		hasRelativePath = 0x08
		hasWorkingDir   = 0x10
		hasArguments    = 0x20
		isUnicode       = 0x80
	)
	charSize := 1
	if flags&isUnicode != 0 {
		charSize = 2
	}
	off := 0
	for _, bit := range []uint32{hasName, hasRelativePath, hasWorkingDir, hasArguments} {
		if flags&bit == 0 {
			continue
		}
		if off+2 > len(sd) {
			return ""
		}
		n := int(binary.LittleEndian.Uint16(sd[off:])) * charSize
		off += 2
		if off+n > len(sd) {
			return ""
		}
		if bit == hasArguments {
			s := sd[off : off+n]
			if charSize == 1 {
				return string(s)
			}
			u := make([]uint16, len(s)/2)
			for i := range u {
				u[i] = binary.LittleEndian.Uint16(s[2*i:])
			}
			return string(utf16.Decode(u))
		}
		off += n
	}
	return ""
}

func joinTarget(base, suffix string) string {
	if suffix == "" {
		return base
	}
	return strings.TrimRight(base, `\`) + `\` + suffix
}

// ansiAt reads a NUL-terminated single-byte string at off (bytes as-is: only
// the file name's extension and ASCII name matter to the caller).
func ansiAt(b []byte, off uint32) string {
	if off == 0 || int(off) >= len(b) {
		return ""
	}
	s := b[off:]
	if i := strings.IndexByte(string(s), 0); i >= 0 {
		s = s[:i]
	}
	return string(s)
}

// utf16At reads a NUL-terminated UTF-16LE string at off.
func utf16At(b []byte, off uint32) string {
	if off == 0 || int(off) >= len(b) {
		return ""
	}
	var u []uint16
	for i := int(off); i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}
