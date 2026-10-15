package main

// Rotating file logger, adapted from linux/log.go: the same line format, a
// 5 MB cap with one backup. Lines also go to stdout, which systemd (journald)
// and launchd capture; a Windows service has no stdout, so there the file
// (and the Event Log, for start/stop/errors) is what admins read.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const maxLogBytes = 5 << 20

var (
	logMu     sync.Mutex
	logFile   *os.File
	logPath   string
	logStdout io.Writer = os.Stdout
)

// initLog opens the log file (creating its directory). Failure is not fatal:
// logging then only goes to stdout.
func initLog(path string) error {
	logMu.Lock()
	defer logMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	logFile, logPath = f, path
	return nil
}

func closeLog() {
	logMu.Lock()
	defer logMu.Unlock()
	if logFile != nil {
		logFile.Close()
		logFile = nil
	}
}

// logf writes one timestamped line.
func logf(format string, args ...any) {
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	logMu.Lock()
	defer logMu.Unlock()
	if logStdout != nil {
		io.WriteString(logStdout, line)
	}
	if logFile == nil {
		return
	}
	if fi, err := logFile.Stat(); err == nil && fi.Size()+int64(len(line)) > maxLogBytes {
		rotateLogLocked()
	}
	if logFile != nil {
		logFile.WriteString(line)
	}
}

// rotateLogLocked moves the log to <path>.1 and starts a new one.
func rotateLogLocked() {
	logFile.Close()
	logFile = nil
	_ = os.Rename(logPath, logPath+".1")
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		logFile = f
	}
}
