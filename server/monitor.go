package main

// The decision engine, adapted from linux/monitor.go evaluate: a process that
// stays above the CPU threshold for kill_duration seconds is killed (enforce
// mode) or reported (monitor mode). Everything OS-specific is behind the
// function fields of Monitor, so the rules are unit-tested on every OS.

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// HotProcess is a process that has been above the threshold.
type HotProcess struct {
	Process
	Exe      string // executable file name ("" if unknown)
	User     string // owner, resolved only when a user rule needs it
	HotSince time.Time
}

// DisplayName is a human-friendly name for logs: the executable when the short
// name is just its truncation, otherwise "name (exe)".
func (h *HotProcess) DisplayName() string {
	switch {
	case h.Exe == "" || strings.EqualFold(h.Exe, h.Name):
		return h.Name
	case strings.HasPrefix(strings.ToLower(h.Exe), strings.ToLower(h.Name)):
		return h.Exe
	default:
		return fmt.Sprintf("%s (%s)", h.Name, h.Exe)
	}
}

// Event is something the monitor did, kept for `hotfixd status` and sent to
// the webhook.
type Event struct {
	Time   time.Time `json:"time"`
	Action string    `json:"action"` // killed | would_kill | kill_failed | backoff
	PID    int       `json:"pid"`
	Name   string    `json:"name"`
	User   string    `json:"user,omitempty"`
	CPU    float64   `json:"cpu"`
	HotFor float64   `json:"hot_for_seconds"`
	Mode   string    `json:"mode"`
	Error  string    `json:"error,omitempty"`
}

const (
	maxEvents = 50

	// Restart-loop backoff: a service manager restarts what we kill, so a name
	// killed backoffKills times within backoffWindow is left alone for the
	// rest of the window instead of being killed every few minutes forever.
	backoffKills  = 3
	backoffWindow = time.Hour
)

// Monitor holds the hot-process state between samples.
type Monitor struct {
	selfPID  int
	selfUID  uint32
	elevated bool

	exeName     func(pid int) string
	userName    func(p Process) string
	inContainer func(pid int) bool
	kill        func(h *HotProcess, grace time.Duration) error
	now         func() time.Time
	onEvent     func(Event)

	// monitorNote, when set, explains why enforce mode is reporting only
	// (no license); set by the engine before each Step.
	monitorNote string

	mu        sync.Mutex
	hot       map[procKey]*HotProcess
	kills     map[string][]time.Time // name → recent kill times
	backedOff map[string]time.Time   // name → when its backoff was last logged
	events    []Event
}

func newMonitor(selfPID int, selfUID uint32, elevated bool) *Monitor {
	return &Monitor{
		selfPID:     selfPID,
		selfUID:     selfUID,
		elevated:    elevated,
		exeName:     exeName,
		userName:    processUser,
		inContainer: inContainer,
		kill:        killProcess,
		now:         time.Now,
		hot:         map[procKey]*HotProcess{},
		kills:       map[string][]time.Time{},
		backedOff:   map[string]time.Time{},
	}
}

// parentInfo is a process's parent, for spotting hotfixd's own process tree.
type parentInfo struct {
	PPID  int
	Start uint64
}

func parentMap(procs []Process) map[int]parentInfo {
	m := make(map[int]parentInfo, len(procs))
	for _, p := range procs {
		m[p.PID] = parentInfo{p.PPID, p.StartTime}
	}
	return m
}

// isDescendant reports whether pid is a child, grandchild, … of ancestor. A
// parent that started after its child is a reused PID, not the real parent
// (Windows keeps stale parent PIDs), so the walk stops there.
func isDescendant(pid, ancestor int, parents map[int]parentInfo) bool {
	cur, ok := parents[pid]
	if !ok {
		return false
	}
	for i := 0; i < 64; i++ {
		if cur.PPID <= 0 {
			return false
		}
		next, ok := parents[cur.PPID]
		if ok && cur.Start != 0 && next.Start > cur.Start {
			return false // the "parent" started later: its PID was reused
		}
		if cur.PPID == ancestor {
			return true
		}
		if !ok {
			return false
		}
		cur = next
	}
	return false
}

// skipReason returns why p must not be killed, or "" if it is a candidate.
// exe/user may be pre-resolved (cached from an earlier sample); they are
// resolved here otherwise.
func (m *Monitor) skipReason(p Process, cfg Config, exe, user *string, parents map[int]parentInfo) string {
	switch {
	case p.KernelThread || p.PID <= maxReservedPID:
		return "system"
	case p.PID == m.selfPID || isDescendant(p.PID, m.selfPID, parents):
		return "hotfixd itself"
	case !m.elevated && unixLike && p.UID != m.selfUID:
		return "another user's process (hotfixd is not running as root)"
	}

	if *exe == "" {
		*exe = m.exeName(p.PID)
	}
	if isProtected(p.Name) || (*exe != "" && isProtected(*exe)) {
		return "protected"
	}
	wl := nameSet(cfg.Whitelist)
	if wl[normalizeName(p.Name)] || (*exe != "" && wl[normalizeName(*exe)]) {
		return "whitelisted"
	}

	if len(cfg.ProtectUsers) > 0 || len(cfg.OnlyUsers) > 0 {
		if *user == "" {
			*user = m.userName(p)
		}
		if userIn(*user, cfg.ProtectUsers) {
			return "protected user"
		}
		if len(cfg.OnlyUsers) > 0 && !userIn(*user, cfg.OnlyUsers) {
			return "not in only_users"
		}
	}
	if cfg.SkipContainers && m.inContainer(p.PID) {
		return "in a container"
	}
	return ""
}

// userIn matches a user name case-insensitively; a Windows "DOMAIN\name"
// matches either the full form or just "name".
func userIn(user string, list []string) bool {
	if user == "" {
		return false
	}
	short := user
	if i := strings.LastIndexByte(user, '\\'); i >= 0 {
		short = user[i+1:]
	}
	for _, u := range list {
		if strings.EqualFold(u, user) || strings.EqualFold(u, short) {
			return true
		}
	}
	return false
}

// Evaluate updates the hot set from one sample and returns the processes that
// have now been hot for at least kill_duration (removed from the hot set, so
// they're reported once per kill_duration, not every sample).
func (m *Monitor) Evaluate(procs []Process, cfg Config) []*HotProcess {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	parents := parentMap(procs)
	seen := map[procKey]bool{}
	var due []*HotProcess

	for _, p := range procs {
		if p.CPU < cfg.CPUThreshold {
			continue
		}
		key := p.key()
		h, tracked := m.hot[key]
		var exe, user string
		if tracked {
			exe, user = h.Exe, h.User
		}
		if m.skipReason(p, cfg, &exe, &user, parents) != "" {
			delete(m.hot, key)
			continue
		}
		seen[key] = true
		if !tracked {
			h = &HotProcess{Process: p, Exe: exe, User: user, HotSince: now}
			m.hot[key] = h
			logf("monitor: %s (PID %d) is above %.0f%% CPU (%.1f%%)", h.DisplayName(), p.PID, cfg.CPUThreshold, p.CPU)
		} else {
			h.CPU, h.User = p.CPU, user
		}
		if now.Sub(h.HotSince).Seconds() >= cfg.KillDuration {
			due = append(due, h)
			delete(m.hot, key)
		}
	}
	for k := range m.hot {
		if !seen[k] {
			delete(m.hot, k)
		}
	}
	return due
}

// Step evaluates one sample and acts on what's due.
func (m *Monitor) Step(procs []Process, cfg Config) {
	for _, h := range m.Evaluate(procs, cfg) {
		m.act(h, cfg)
	}
}

func (m *Monitor) act(h *HotProcess, cfg Config) {
	now := m.now()
	ev := Event{
		Time: now, PID: h.PID, Name: h.DisplayName(), User: h.User, CPU: h.CPU,
		HotFor: now.Sub(h.HotSince).Seconds(), Mode: cfg.Mode,
	}

	if cfg.Mode != ModeEnforce {
		ev.Action = "would_kill"
		why := "monitor mode; run `hotfixd config set mode enforce` to kill"
		if m.monitorNote != "" {
			why = m.monitorNote
		}
		logf("monitor: would kill %s (PID %d): %.1f%% CPU for %.0fs (%s)", ev.Name, h.PID, h.CPU, ev.HotFor, why)
		m.record(ev)
		return
	}

	nameKey := normalizeName(h.Name)
	if m.inBackoff(nameKey, now) {
		m.mu.Lock()
		last, logged := m.backedOff[nameKey]
		if !logged || now.Sub(last) >= backoffWindow {
			m.backedOff[nameKey] = now
			m.mu.Unlock()
			ev.Action = "backoff"
			logf("monitor: %s keeps coming back (killed %d times in the last hour); leaving it alone for now. Exclude it or fix whatever restarts it.",
				ev.Name, backoffKills)
			m.record(ev)
			return
		}
		m.mu.Unlock()
		return
	}

	logf("monitor: killing %s (PID %d): %.1f%% CPU for %.0fs", ev.Name, h.PID, h.CPU, ev.HotFor)
	if err := m.kill(h, time.Duration(cfg.TermGraceSeconds*float64(time.Second))); err != nil {
		ev.Action, ev.Error = "kill_failed", err.Error()
		logf("monitor: could not kill %s (PID %d): %v", ev.Name, h.PID, err)
	} else {
		ev.Action = "killed"
		m.mu.Lock()
		m.kills[nameKey] = append(m.kills[nameKey], now)
		m.mu.Unlock()
	}
	m.record(ev)
}

// inBackoff reports whether name was killed backoffKills times within the
// window (pruning older kills).
func (m *Monitor) inBackoff(name string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	recent := m.kills[name][:0]
	for _, t := range m.kills[name] {
		if now.Sub(t) < backoffWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		delete(m.kills, name)
		return false
	}
	m.kills[name] = recent
	return len(recent) >= backoffKills
}

func (m *Monitor) record(ev Event) {
	m.mu.Lock()
	m.events = append(m.events, ev)
	if len(m.events) > maxEvents {
		m.events = m.events[len(m.events)-maxEvents:]
	}
	m.mu.Unlock()
	if m.onEvent != nil {
		m.onEvent(ev)
	}
}

// Snapshot returns the current hot processes (hottest first) and recent events
// (newest first), for status.json.
func (m *Monitor) Snapshot() ([]HotProcess, []Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	hot := make([]HotProcess, 0, len(m.hot))
	for _, h := range m.hot {
		hot = append(hot, *h)
	}
	sort.Slice(hot, func(i, j int) bool { return hot[i].CPU > hot[j].CPU })
	events := make([]Event, len(m.events))
	for i, e := range m.events {
		events[len(m.events)-1-i] = e
	}
	return hot, events
}

// Reset forgets every hot process (e.g. after a config change to the rules).
func (m *Monitor) Reset() {
	m.mu.Lock()
	m.hot = map[procKey]*HotProcess{}
	m.mu.Unlock()
}
