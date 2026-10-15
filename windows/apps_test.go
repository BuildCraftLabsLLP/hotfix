//go:build windows

package main

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestGroupApps_MergesSortsAndMeasuresCPU(t *testing.T) {
	procs := []appProc{
		{pid: 100, ppid: 1, name: "chrome"},
		{pid: 101, ppid: 100, name: "chrome"},
		{pid: 200, ppid: 1, name: "Notepad"},
		{pid: 300, ppid: 1, name: "code"},
		{pid: 400, ppid: 1, name: "CHROME"}, // same app, different case
	}
	// 250ms = 2,500,000 × 100ns.
	before := map[uint32]cpuSample{
		100: {created: 1, total: 0},
		101: {created: 1, total: 0},
		200: {created: 1, total: 0},
		300: {created: 1, total: 0},
		400: {created: 1, total: 0},
	}
	after := map[uint32]cpuSample{
		100: {created: 1, total: 1_250_000},  // 50%
		101: {created: 1, total: 1_250_000},  // 50%
		200: {created: 1, total: 250_000},    // 10%
		300: {created: 9, total: 99_999_999}, // PID reused: ignored
		400: {created: 1, total: 500_000},    // 20%
	}
	apps := groupApps(procs, before, after, 250*time.Millisecond)

	if len(apps) != 3 {
		t.Fatalf("want 3 apps (chrome merged), got %+v", apps)
	}
	if apps[0].Name != "chrome" || apps[0].CPU < 119.9 || apps[0].CPU > 120.1 {
		t.Errorf("busiest first, chrome summed to 120%%: got %+v", apps[0])
	}
	if apps[1].Name != "Notepad" || apps[1].CPU < 9.9 || apps[1].CPU > 10.1 {
		t.Errorf("second: %+v", apps[1])
	}
	if apps[2].Name != "code" || apps[2].CPU != 0 {
		t.Errorf("a reused PID must not count: %+v", apps[2])
	}
}

func TestGroupApps_SkipsOwnChildren(t *testing.T) {
	self := windows.GetCurrentProcessId()
	procs := []appProc{
		{pid: 500, ppid: self, name: "msedgewebview2"}, // the settings window's WebView2
		{pid: 501, ppid: 500, name: "msedgewebview2"},
		{pid: 600, ppid: 1, name: "steam"},
	}
	apps := groupApps(procs, nil, nil, 250*time.Millisecond)
	if len(apps) != 1 || apps[0].Name != "steam" {
		t.Errorf("Hotfix's own child processes must be left out, got %+v", apps)
	}
}

// Runs against the real process list.
func TestListRunningApps_Live(t *testing.T) {
	apps, err := listRunningApps()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) == 0 {
		t.Fatal("expected at least one running app in this session")
	}
	seen := map[string]bool{}
	for _, a := range apps {
		low := strings.ToLower(a.Name)
		if strings.HasSuffix(low, ".exe") {
			t.Errorf("names must not carry .exe: %q", a.Name)
		}
		if isProtected(a.Name) {
			t.Errorf("protected process listed: %q", a.Name)
		}
		if seen[low] {
			t.Errorf("duplicate entry: %q", a.Name)
		}
		seen[low] = true
		if a.CPU < 0 {
			t.Errorf("negative CPU for %q", a.Name)
		}
	}
}
