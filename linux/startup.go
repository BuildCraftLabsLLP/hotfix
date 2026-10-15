//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Per-user autostart is an XDG autostart entry — a .desktop file in
// ~/.config/autostart that every mainstream desktop environment runs at login.
// install.sh writes the same file when run with --autostart; managing it here
// keeps the tray toggle and the installer in sync (the file is the single
// source of truth for the state).
const autostartFileName = "hotfix.desktop"

func autostartPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "autostart", autostartFileName), nil
}

// launchAtLoginEnabled reports whether the autostart entry exists and is not
// disabled (desktops "remove" an autostart entry by setting Hidden=true).
func launchAtLoginEnabled() bool {
	path, err := autostartPath()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.EqualFold(strings.ReplaceAll(strings.TrimSpace(line), " ", ""), "hidden=true") {
			return false
		}
	}
	return true
}

// setLaunchAtLogin adds or removes the autostart entry, pointing it at the
// currently running executable.
func setLaunchAtLogin(enabled bool) error {
	path, err := autostartPath()
	if err != nil {
		return fmt.Errorf("resolve autostart path: %w", err)
	}

	if !enabled {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove autostart entry: %w", err)
		}
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve exe path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create autostart dir: %w", err)
	}
	return os.WriteFile(path, []byte(autostartEntry(exe)), 0644)
}

// autostartEntry renders the .desktop file contents for exe.
func autostartEntry(exe string) string {
	return "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=Hotfix\n" +
		"Comment=Kills runaway processes before your fans spin up\n" +
		"Exec=" + desktopQuote(exe) + "\n" +
		"Icon=hotfix\n" +
		"Terminal=false\n" +
		"X-GNOME-Autostart-enabled=true\n"
}

// desktopQuote quotes a path for a .desktop Exec= line per the Desktop Entry
// spec: wrapped in double quotes with `"`, "`", `$` and `\` backslash-escaped,
// and a literal % doubled (it otherwise introduces a field code).
func desktopQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '`', '$', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '%':
			b.WriteString("%%")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
