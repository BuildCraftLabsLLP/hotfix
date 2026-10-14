//go:build linux

package main

import (
	"sort"
	"strings"
	"time"
)

// runningApp is one entry in the settings page's running-apps dropdown
// (Exclusions card), returned by the hotfixListProcesses binding.
type runningApp struct {
	Name string  `json:"name"`
	CPU  float64 `json:"cpu"` // percent of one core, summed over the app's processes
}

// appSampleInterval is how long CPU time is sampled to rank the apps.
const appSampleInterval = 250 * time.Millisecond

// listRunningApps returns this user's running apps, busiest first, for the
// exclusions dropdown. Hotfix itself, kernel threads and the always-protected
// session processes are left out — they're never killed anyway.
func listRunningApps() ([]runningApp, error) {
	first, err := readProcSamples()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	time.Sleep(appSampleInterval)
	second, err := readProcSamples()
	if err != nil {
		return nil, err
	}
	entries := cpuUsage(first, second, time.Since(start).Seconds(), clockTicks())
	return groupApps(entries, exeBaseName), nil
}

// groupApps names each process by its executable (falling back to the task
// name), merges processes by name — a browser is one entry, not twenty — sums
// their CPU, and sorts busiest first, then by name. Exclusions match the
// executable name too, so picking "firefox" covers all of its processes.
func groupApps(entries []processEntry, exeName func(pid int) string) []runningApp {
	parents := make(map[int]int, len(entries))
	for _, e := range entries {
		parents[e.PID] = e.PPID
	}

	byName := map[string]*runningApp{}
	var order []string
	for _, e := range entries {
		if e.PID <= 2 || e.PPID == 2 || e.PID == selfPID || isDescendant(e.PID, selfPID, parents) {
			continue
		}
		if selfUID != 0 && e.UID != selfUID {
			continue
		}
		name := exeName(e.PID)
		if name == "" {
			name = e.Name
		}
		if name == "" || name == "hotfix" || isProtected(name) || isProtected(e.Name) {
			continue
		}
		key := strings.ToLower(name)
		app, ok := byName[key]
		if !ok {
			app = &runningApp{Name: name}
			byName[key] = app
			order = append(order, key)
		}
		app.CPU += e.CPU
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
