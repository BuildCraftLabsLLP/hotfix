//go:build linux

package main

import (
	"strings"
	"testing"
)

func TestGroupApps_MergesSortsAndNames(t *testing.T) {
	exes := map[int]string{
		100: "firefox", 101: "firefox", 102: "firefox",
		200: "code",
		400: "Xorg",
	}
	exe := func(pid int) string { return exes[pid] }
	entries := []processEntry{
		{PID: 100, PPID: 1000, Name: "firefox", CPU: 20, UID: selfUID},
		{PID: 101, PPID: 100, Name: "Isolated Web Co", CPU: 30, UID: selfUID}, // child, named by exe
		{PID: 102, PPID: 100, Name: "Web Content", CPU: 5, UID: selfUID},
		{PID: 200, PPID: 1000, Name: "code", CPU: 10, UID: selfUID},
		{PID: 300, PPID: 1000, Name: "myscript", CPU: 2, UID: selfUID}, // exe unreadable: task name
		{PID: 400, PPID: 1000, Name: "Xorg", CPU: 90, UID: selfUID},    // protected
		{PID: 500, PPID: 2, Name: "kworker/0:1", CPU: 50, UID: 0},      // kernel thread
	}
	apps := groupApps(entries, exe)

	names := make([]string, len(apps))
	for i, a := range apps {
		names[i] = a.Name
	}
	if got := strings.Join(names, ","); got != "firefox,code,myscript" {
		t.Fatalf("got %s, want firefox,code,myscript", got)
	}
	if apps[0].CPU != 55 {
		t.Errorf("firefox CPU should be summed over its processes: %v", apps[0].CPU)
	}
}

func TestGroupApps_SkipsHotfixAndOtherUsers(t *testing.T) {
	if selfUID == 0 {
		t.Skip("as root every user's processes are listed")
	}
	exes := map[int]string{7000: "hotfix", 7001: "WebKitWebProcess", 8000: "postgres", 9000: "steam"}
	entries := []processEntry{
		{PID: 7000, PPID: selfPID, Name: "hotfix", CPU: 5, UID: selfUID},        // settings window
		{PID: 7001, PPID: 7000, Name: "WebKitWebProces", CPU: 80, UID: selfUID}, // its renderer
		{PID: selfPID, PPID: 1, Name: "hotfix", CPU: 1, UID: selfUID},           // this process
		{PID: 8000, PPID: 1, Name: "postgres", CPU: 40, UID: selfUID + 1},       // another user
		{PID: 9000, PPID: 1, Name: "steam", CPU: 3, UID: selfUID},
	}
	apps := groupApps(entries, func(pid int) string { return exes[pid] })
	if len(apps) != 1 || apps[0].Name != "steam" {
		t.Errorf("want only steam, got %+v", apps)
	}
}

// Runs against the real /proc: at least the test runner's shell or go itself
// should show up, and nothing protected or duplicated.
func TestListRunningApps_Live(t *testing.T) {
	apps, err := listRunningApps()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, a := range apps {
		low := strings.ToLower(a.Name)
		if seen[low] {
			t.Errorf("duplicate entry %q", a.Name)
		}
		seen[low] = true
		if isProtected(a.Name) {
			t.Errorf("protected process listed: %q", a.Name)
		}
		if a.CPU < 0 {
			t.Errorf("negative CPU for %q", a.Name)
		}
	}
}
