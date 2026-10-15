//go:build linux

package main

// Crash reporting, mirroring the Windows crashreport.go: a tray app launched
// from a .desktop entry has no visible stderr, so a panic would otherwise
// vanish. This file captures any panic (with stack trace) to a marker file and,
// on the next launch — or immediately, if the panic was recovered — opens a
// pre-filled GitHub "New Issue" page so the user can submit it with one click.
//
// A pre-filled issue URL is used instead of the GitHub API on purpose: filing
// issues programmatically would require an auth token, which cannot be safely
// embedded in a distributed client. This keeps reporting tokenless and lets the
// user review the report before it is sent.

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

const issueBaseURL = "https://github.com/BuildCraftLabsLLP/hotfix/issues/new"

// reportOnce ensures we open at most one browser tab per process run, so a
// tight panic loop can't spawn dozens of GitHub tabs.
var reportOnce sync.Once

// crashFilePath returns the path to the pending-crash marker file, or "" if the
// state dir cannot be determined.
func crashFilePath() string {
	dir := stateDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "lastcrash.txt")
}

// recordCrash persists a panic value and stack trace so it can be reported.
func recordCrash(where string, r any) {
	entry := fmt.Sprintf("[%s] panic in %s: %v\n\n%s\n",
		time.Now().Format(time.RFC3339), where, r, debug.Stack())
	logf("CRASH panic in %s: %v", where, r)
	if p := crashFilePath(); p != "" {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(entry), 0o644)
	}
}

// safe runs fn synchronously, converting any panic into a crash report instead
// of taking the whole process down.
func safe(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			recordCrash(name, r)
			reportPendingCrash()
		}
	}()
	fn()
}

// safeGo runs fn in a new goroutine guarded by safe.
func safeGo(name string, fn func()) {
	go safe(name, fn)
}

// reportPendingCrash opens a pre-filled GitHub issue if a crash marker exists.
// Called at startup (for crashes that killed the process) and right after a
// recovered in-session panic. The marker is always removed so it isn't reported
// twice; the browser is opened at most once per run.
func reportPendingCrash() {
	p := crashFilePath()
	if p == "" {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil || len(data) == 0 {
		return
	}
	reportOnce.Do(func() {
		openURL(buildIssueURL(string(data)))
	})
	_ = os.Remove(p)
}

// buildIssueURL constructs a GitHub New Issue URL pre-filled with the crash.
func buildIssueURL(crash string) string {
	title := fmt.Sprintf("Crash report — Hotfix v%s (Linux/%s)", currentVersion, runtime.GOARCH)

	var b strings.Builder
	b.WriteString("_Automated crash report — please review and remove anything sensitive before submitting._\n\n")
	b.WriteString(fmt.Sprintf("- **Version:** %s\n- **OS:** Linux (%s)\n- **Session:** %s\n\n",
		currentVersion, runtime.GOARCH, sessionDescription()))
	b.WriteString("### Panic / stack trace\n```\n")
	b.WriteString(truncate(crash, 3500))
	b.WriteString("\n```\n")

	if lp := logFilePath(); lp != "" {
		if ld, err := os.ReadFile(lp); err == nil && len(ld) > 0 {
			b.WriteString("\n### Recent log\n```\n")
			b.WriteString(truncate(tailLines(string(ld), 40), 1500))
			b.WriteString("\n```\n")
		}
	}

	q := url.Values{}
	q.Set("title", title)
	q.Set("labels", "crash")
	q.Set("body", b.String())
	return issueBaseURL + "?" + q.Encode()
}

// sessionDescription names the desktop and display protocol (e.g. "GNOME /
// wayland") — the main variable in Linux tray and focus behavior.
func sessionDescription() string {
	desktop := os.Getenv("XDG_CURRENT_DESKTOP")
	if desktop == "" {
		desktop = "unknown desktop"
	}
	session := os.Getenv("XDG_SESSION_TYPE")
	if session == "" {
		session = "unknown session type"
	}
	return desktop + " / " + session
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n…(truncated)"
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
