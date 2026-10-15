//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// --- Logging ---

// maxLogBytes caps the log file size; when exceeded it is rolled to hotfix.log.1
// (one backup) and a fresh log is started, so the file never grows unbounded.
const maxLogBytes = 5 * 1024 * 1024

var (
	logFile  *os.File
	logMu    sync.Mutex
	logBytes int64 // approximate current size of logFile, guarded by logMu
)

// stateDir returns the per-user directory for runtime state (log, crash marker,
// instance lock): $XDG_STATE_HOME/hotfix, defaulting to ~/.local/state/hotfix.
// Returns "" if it cannot be determined.
func stateDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" || !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "hotfix")
}

// logFilePath returns the absolute path to the log file, or "" if it cannot be
// determined.
func logFilePath() string {
	dir := stateDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "hotfix.log")
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
		logFile = nil
	}
}
