//go:build windows

package main

import (
	"sort"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// runningApp is a running app for the exclusions dropdown (Exclusions card);
// listApps merges these with the installed apps.
type runningApp struct {
	Name string  `json:"name"`
	CPU  float64 `json:"cpu"` // percent of one core, summed over the app's processes
}

// appSampleInterval is how long CPU time is sampled to rank the apps. Short:
// the binding runs on the WebView2 UI thread.
const appSampleInterval = 250 * time.Millisecond

// appProc is one process seen by the snapshot.
type appProc struct {
	pid  uint32
	ppid uint32
	name string // image name without ".exe"
}

// cpuSample is a process's creation time (to detect PID reuse) and total
// kernel+user CPU time, both in 100ns units.
type cpuSample struct{ created, total uint64 }

// listRunningApps returns the apps running in the user's session, busiest
// first, for the exclusions dropdown. Hotfix itself and the always-protected
// system processes are left out — they can't be killed anyway.
func listRunningApps() ([]runningApp, error) {
	procs, err := sessionProcesses()
	if err != nil {
		return nil, err
	}
	before := sampleCPU(procs)
	start := time.Now()
	time.Sleep(appSampleInterval)
	after := sampleCPU(procs)
	return groupApps(procs, before, after, time.Since(start)), nil
}

// sessionProcesses lists the processes in this process's session (the user's
// desktop — not services in session 0), minus Hotfix and protected processes.
func sessionProcesses() ([]appProc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)

	self := windows.GetCurrentProcessId()
	var mySession uint32
	_ = windows.ProcessIdToSessionId(self, &mySession)

	var procs []appProc
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		pid := e.ProcessID
		if pid <= 4 || pid == self {
			continue
		}
		var session uint32
		if windows.ProcessIdToSessionId(pid, &session) != nil || session != mySession {
			continue
		}
		name := windows.UTF16ToString(e.ExeFile[:])
		if len(name) > 4 && strings.EqualFold(name[len(name)-4:], ".exe") {
			name = name[:len(name)-4]
		}
		if name == "" || isProtected(name) {
			continue
		}
		procs = append(procs, appProc{pid: pid, ppid: e.ParentProcessID, name: name})
	}
	return procs, nil
}

// sampleCPU reads each process's CPU time. Processes that can't be opened
// (protected, or gone) are simply missing from the result.
func sampleCPU(procs []appProc) map[uint32]cpuSample {
	out := make(map[uint32]cpuSample, len(procs))
	for _, p := range procs {
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, p.pid)
		if err != nil {
			continue
		}
		var created, exited, kernel, user windows.Filetime
		if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) == nil {
			out[p.pid] = cpuSample{created: ftUint64(created), total: ftUint64(kernel) + ftUint64(user)}
		}
		windows.CloseHandle(h)
	}
	return out
}

func ftUint64(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// groupApps merges processes by name (case-insensitive — a browser is one
// entry, not twenty), sums their CPU over the sample, and sorts busiest
// first, then by name. Hotfix's own child processes are left out.
func groupApps(procs []appProc, before, after map[uint32]cpuSample, elapsed time.Duration) []runningApp {
	self := windows.GetCurrentProcessId()
	parent := make(map[uint32]uint32, len(procs))
	for _, p := range procs {
		parent[p.pid] = p.ppid
	}
	ownChild := func(pid uint32) bool {
		for i := 0; i < 64; i++ {
			pp, ok := parent[pid]
			if !ok || pp == 0 {
				return false
			}
			if pp == self {
				return true
			}
			pid = pp
		}
		return false
	}

	interval := float64(elapsed.Nanoseconds() / 100) // 100ns units
	byName := map[string]*runningApp{}
	var order []string
	for _, p := range procs {
		if ownChild(p.pid) {
			continue
		}
		key := strings.ToLower(p.name)
		app, ok := byName[key]
		if !ok {
			app = &runningApp{Name: p.name}
			byName[key] = app
			order = append(order, key)
		}
		b, ok1 := before[p.pid]
		a, ok2 := after[p.pid]
		if ok1 && ok2 && a.created == b.created && a.total >= b.total && interval > 0 {
			app.CPU += float64(a.total-b.total) / interval * 100
		}
	}

	apps := make([]runningApp, 0, len(order))
	for _, k := range order {
		apps = append(apps, *byName[k])
	}
	sort.SliceStable(apps, func(i, j int) bool {
		if apps[i].CPU != apps[j].CPU {
			return apps[i].CPU > apps[j].CPU
		}
		return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name)
	})
	return apps
}

// appEntry is one row of the exclusions dropdown, returned by the
// hotfixListApps binding: a running app (with its CPU use) or an installed
// one. Name is what gets excluded; Label is the app's friendly name, if known.
type appEntry struct {
	Name    string  `json:"name"`
	Label   string  `json:"label,omitempty"`
	CPU     float64 `json:"cpu"`
	Running bool    `json:"running"`
}

// listApps returns the running apps (busiest first) followed by the installed
// apps that aren't running (by label).
func listApps() ([]appEntry, error) {
	running, err := listRunningApps()
	if err != nil {
		return nil, err
	}
	return mergeApps(running, cachedInstalledApps()), nil
}

// minListedCPU is the CPU use (percent) at which a running process with no
// installed app behind it still makes the list: busy enough to matter.
const minListedCPU = 0.5

// mergeApps combines the running and installed lists: a running app gets its
// installed label, and an installed app that's running isn't listed twice.
// Running processes that are neither a known installed app nor using CPU are
// left out — background helpers that would bury the apps people look for
// (they can still be excluded by typing their name).
func mergeApps(running []runningApp, installed []installedApp) []appEntry {
	labels := make(map[string]string, len(installed))
	for _, a := range installed {
		if _, ok := labels[strings.ToLower(a.Name)]; !ok {
			labels[strings.ToLower(a.Name)] = a.Label
		}
	}
	out := make([]appEntry, 0, len(running)+len(installed))
	listed := make(map[string]bool, len(running))
	for _, r := range running {
		key := strings.ToLower(r.Name)
		if labels[key] == "" && r.CPU < minListedCPU {
			continue
		}
		listed[key] = true
		out = append(out, appEntry{Name: r.Name, Label: labels[key], CPU: r.CPU, Running: true})
	}
	for _, a := range installed {
		key := strings.ToLower(a.Name)
		if listed[key] {
			continue
		}
		listed[key] = true // also dedupes installed entries sharing a program
		out = append(out, appEntry{Name: a.Name, Label: a.Label})
	}
	return out
}
