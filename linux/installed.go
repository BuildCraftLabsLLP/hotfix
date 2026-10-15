//go:build linux

package main

import (
	"bufio"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// installedApp is a desktop application installed on the system, for the
// exclusions dropdown: Name is what an exclusion matches (the executable's
// file name), Label is what the app menu calls it.
type installedApp struct {
	Name  string
	Label string
}

// Limits so a huge or hostile applications directory can't stall the window.
const (
	maxDesktopFiles     = 5000
	maxDesktopFileBytes = 64 << 10
)

// installedApps lists the applications in the app menu — the .desktop
// entries in the XDG data dirs, including Flatpak and Snap exports — sorted
// by label. Entries hidden from the menu, protected processes and Hotfix
// itself are left out; an entry in the user's own directory overrides a
// system one with the same desktop ID.
func installedApps() []installedApp {
	seenID := map[string]bool{}
	seenName := map[string]bool{}
	var apps []installedApp
	files := 0

	for _, root := range applicationDirs() {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable: skip, keep walking
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".desktop") {
				return nil
			}
			if files++; files > maxDesktopFiles {
				return filepath.SkipAll
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			id := strings.ReplaceAll(rel, string(filepath.Separator), "-")
			if seenID[id] {
				return nil
			}
			seenID[id] = true

			f, err := os.Open(path)
			if err != nil {
				return nil
			}
			app, ok := parseDesktopEntry(io.LimitReader(f, maxDesktopFileBytes), flatpakCommand)
			f.Close()
			if !ok {
				return nil
			}
			key := strings.ToLower(app.Name)
			if seenName[key] || app.Name == "hotfix" || isProtected(app.Name) {
				return nil
			}
			seenName[key] = true
			apps = append(apps, app)
			return nil
		})
	}

	sort.SliceStable(apps, func(i, j int) bool {
		return strings.ToLower(apps[i].Label) < strings.ToLower(apps[j].Label)
	})
	return apps
}

// systemAppBases are the Flatpak and Snap export dirs, scanned even when the
// session's XDG_DATA_DIRS leaves them out. A variable so tests can drop them.
var systemAppBases = []string{"/var/lib/flatpak/exports/share", "/var/lib/snapd/desktop"}

// applicationDirs returns the "applications" directories to scan, user first
// so their entries win: $XDG_DATA_HOME, $XDG_DATA_DIRS, plus the Flatpak and
// Snap export dirs in case the session didn't add them.
func applicationDirs() []string {
	home, _ := os.UserHomeDir()
	var bases []string
	if dh := os.Getenv("XDG_DATA_HOME"); dh != "" {
		bases = append(bases, dh)
	} else if home != "" {
		bases = append(bases, filepath.Join(home, ".local", "share"))
	}
	dataDirs := os.Getenv("XDG_DATA_DIRS")
	if dataDirs == "" {
		dataDirs = "/usr/local/share:/usr/share"
	}
	bases = append(bases, filepath.SplitList(dataDirs)...)
	if home != "" {
		bases = append(bases, filepath.Join(home, ".local", "share", "flatpak", "exports", "share"))
	}
	bases = append(bases, systemAppBases...)

	var dirs []string
	seen := map[string]bool{}
	for _, b := range bases {
		if b == "" || !filepath.IsAbs(b) {
			continue
		}
		d := filepath.Join(filepath.Clean(b), "applications")
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// parseDesktopEntry reads the [Desktop Entry] group of a .desktop file and
// returns the app it launches. ok is false for entries that aren't shown in
// the app menu or whose program can't be worked out.
func parseDesktopEntry(r io.Reader, flatpakCmd func(appID string) string) (installedApp, bool) {
	var name, exec, tryExec, typ string
	hidden := false
	inEntry := false

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxDesktopFileBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] == '[' {
			inEntry = line == "[Desktop Entry]"
			continue
		}
		if !inEntry {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "Type":
			typ = value
		case "Name":
			name = unescapeDesktopString(value)
		case "Exec":
			exec = value
		case "TryExec":
			tryExec = unescapeDesktopString(value)
		case "NoDisplay", "Hidden":
			if strings.EqualFold(value, "true") {
				hidden = true
			}
		}
	}
	if typ != "Application" || hidden || name == "" {
		return installedApp{}, false
	}
	prog := execProgram(exec, tryExec, flatpakCmd)
	if prog == "" {
		return installedApp{}, false
	}
	return installedApp{Name: prog, Label: name}, true
}

// unescapeDesktopString undoes the Desktop Entry string escapes.
func unescapeDesktopString(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 's':
				b.WriteByte(' ')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// splitExec splits an Exec value into arguments per the Desktop Entry spec:
// space-separated, double quotes group, backslash escapes inside quotes, and
// field codes (%u, %F, …) dropped.
func splitExec(exec string) []string {
	exec = unescapeDesktopString(exec)
	var args []string
	var cur strings.Builder
	inQuote, quoted := false, false
	flush := func() {
		if cur.Len() > 0 || quoted {
			arg := cur.String()
			if !(len(arg) == 2 && arg[0] == '%') {
				args = append(args, arg)
			}
		}
		cur.Reset()
		quoted = false
	}
	for i := 0; i < len(exec); i++ {
		c := exec[i]
		switch {
		case inQuote && c == '\\' && i+1 < len(exec):
			i++
			cur.WriteByte(exec[i])
		case c == '"':
			inQuote = !inQuote
			quoted = true
		case !inQuote && (c == ' ' || c == '\t'):
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return args
}

// execProgram works out the process name an Exec line ends up running:
// "env VAR=1 /usr/bin/foo %U" → "foo"; "flatpak run org.mozilla.firefox" →
// the Flatpak's command ("firefox"); "/snap/bin/spotify" → "spotify". A shell
// wrapper ("sh -c …") falls back to TryExec, or gives up.
func execProgram(exec, tryExec string, flatpakCmd func(appID string) string) string {
	args := splitExec(exec)
	for len(args) > 0 {
		base := filepath.Base(args[0])
		switch {
		case base == "env" || isEnvAssignment(args[0]):
			args = args[1:] // env and VAR=value prefixes
			continue
		case base == "flatpak":
			return flatpakProgram(args[1:], flatpakCmd)
		case base == "snap" && len(args) > 2 && args[1] == "run":
			return args[2]
		case base == "sh" || base == "bash" || base == "dash" || base == "zsh":
			if tryExec != "" {
				return filepath.Base(tryExec)
			}
			return ""
		}
		return base
	}
	if tryExec != "" {
		return filepath.Base(tryExec)
	}
	return ""
}

// isEnvAssignment reports whether arg is a NAME=value prefix, as in
// "env BAMF_DESKTOP_FILE_HINT=/var/lib/snapd/desktop/… /snap/bin/app".
func isEnvAssignment(arg string) bool {
	i := strings.IndexByte(arg, '=')
	if i <= 0 {
		return false
	}
	for j := 0; j < i; j++ {
		c := arg[j]
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || j > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// flatpakProgram finds the command a "flatpak run [options] APP-ID" starts:
// --command=X if given, else the app's declared command, else the last part
// of the app ID ("org.gnome.Calculator" → "calculator").
func flatpakProgram(args []string, flatpakCmd func(appID string) string) string {
	run := false
	command, appID := "", ""
	for _, a := range args {
		switch {
		case !run:
			run = a == "run"
		case strings.HasPrefix(a, "--command="):
			command = strings.TrimPrefix(a, "--command=")
		case strings.HasPrefix(a, "-"):
			// other options
		case appID == "":
			appID = a
		}
	}
	switch {
	case command != "":
		return filepath.Base(command)
	case appID == "":
		return ""
	}
	if flatpakCmd != nil {
		if c := flatpakCmd(appID); c != "" {
			return filepath.Base(c)
		}
	}
	return strings.ToLower(appID[strings.LastIndexByte(appID, '.')+1:])
}

// flatpakCommand reads an installed Flatpak's declared command from its
// metadata ([Application] command=…), user installation first.
func flatpakCommand(appID string) string {
	if appID == "" || strings.ContainsAny(appID, `/\`) || strings.Contains(appID, "..") {
		return ""
	}
	var roots []string
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, ".local", "share", "flatpak"))
	}
	roots = append(roots, "/var/lib/flatpak")
	for _, root := range roots {
		f, err := os.Open(filepath.Join(root, "app", appID, "current", "active", "metadata"))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(io.LimitReader(f, maxDesktopFileBytes))
		inApp := false
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "[") {
				inApp = line == "[Application]"
				continue
			}
			if inApp && strings.HasPrefix(line, "command=") {
				f.Close()
				return strings.TrimSpace(strings.TrimPrefix(line, "command="))
			}
		}
		f.Close()
	}
	return ""
}
