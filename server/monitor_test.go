package main

import (
	"errors"
	"testing"
	"time"
)

// testMonitor returns a Monitor with fake OS hooks and a controllable clock.
func testMonitor(t *testing.T) (*Monitor, *time.Time, *[]int) {
	t.Helper()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	var killed []int
	m := newMonitor(1000, 0, true)
	m.now = func() time.Time { return now }
	m.exeName = func(pid int) string { return "" }
	m.userName = func(p Process) string { return map[uint32]string{0: "root", 26: "postgres", 1001: "alice"}[p.UID] }
	m.inContainer = func(pid int) bool { return pid == 777 }
	m.kill = func(h *HotProcess, _ time.Duration) error {
		killed = append(killed, h.PID)
		return nil
	}
	return m, &now, &killed
}

func enforceCfg() Config {
	c := defaultConfig()
	c.Mode = ModeEnforce
	c.CPUThreshold = 90
	c.KillDuration = 60
	return c
}

func proc(pid int, name string, cpu float64) Process {
	return Process{PID: pid, PPID: 1, Name: name, CPU: cpu, StartTime: uint64(pid) * 10, UID: 1001}
}

// run feeds the same sample every 5s for d and returns what was killed.
func run(m *Monitor, now *time.Time, cfg Config, d time.Duration, procs ...Process) {
	for elapsed := time.Duration(0); elapsed <= d; elapsed += pollInterval {
		m.Step(procs, cfg)
		*now = now.Add(pollInterval)
	}
}

func TestKillsAfterDurationOnly(t *testing.T) {
	m, now, killed := testMonitor(t)
	cfg := enforceCfg()
	run(m, now, cfg, 55*time.Second, proc(500, "miner", 150))
	if len(*killed) != 0 {
		t.Fatalf("killed before kill_duration: %v", *killed)
	}
	run(m, now, cfg, 5*time.Second, proc(500, "miner", 150))
	if len(*killed) != 1 || (*killed)[0] != 500 {
		t.Fatalf("want PID 500 killed at 60s, got %v", *killed)
	}
}

func TestCoolingDownResetsTheClock(t *testing.T) {
	m, now, killed := testMonitor(t)
	cfg := enforceCfg()
	run(m, now, cfg, 50*time.Second, proc(500, "build", 95))
	run(m, now, cfg, 0, proc(500, "build", 10)) // one quiet sample
	run(m, now, cfg, 50*time.Second, proc(500, "build", 95))
	if len(*killed) != 0 {
		t.Fatalf("a process that dipped below the threshold must start over: %v", *killed)
	}
}

func TestMonitorModeOnlyReports(t *testing.T) {
	m, now, killed := testMonitor(t)
	cfg := enforceCfg()
	cfg.Mode = ModeMonitor
	run(m, now, cfg, 65*time.Second, proc(500, "miner", 150))
	if len(*killed) != 0 {
		t.Fatalf("monitor mode must never kill: %v", *killed)
	}
	_, events := m.Snapshot()
	if len(events) != 1 || events[0].Action != "would_kill" || events[0].PID != 500 {
		t.Fatalf("want one would_kill event, got %+v", events)
	}
}

func TestPIDReuseIsANewProcess(t *testing.T) {
	m, now, killed := testMonitor(t)
	cfg := enforceCfg()
	run(m, now, cfg, 50*time.Second, proc(500, "old", 99))
	reused := proc(500, "new", 99)
	reused.StartTime = 999999
	run(m, now, cfg, 15*time.Second, reused)
	if len(*killed) != 0 {
		t.Fatalf("a reused PID must not inherit the old process's hot time: %v", *killed)
	}
}

func TestNeverKilled(t *testing.T) {
	cfg := enforceCfg()
	cfg.Whitelist = []string{"Backup-Tool"}
	cfg.ProtectUsers = []string{"postgres"}
	cfg.SkipContainers = true

	kernel := proc(30, "kworker/0:1", 100)
	kernel.KernelThread = true
	self := proc(1000, "hotfixd", 100)
	child := proc(1001, "helper", 100)
	child.PPID = 1000
	db := proc(600, "postgres", 100)
	db.UID = 26
	container := proc(777, "app", 100)

	cases := map[string]Process{
		"kernel thread":  kernel,
		"reserved PID":   proc(1, "init", 100),
		"hotfixd itself": self,
		"its child":      child,
		"protected name": proc(400, "hotfixd", 100),
		"whitelisted":    proc(401, "backup-tool", 100),
		"protected user": db,
		"in a container": container,
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			m, now, killed := testMonitor(t)
			run(m, now, cfg, 2*time.Minute, p, self)
			if len(*killed) != 0 {
				t.Fatalf("killed %v", *killed)
			}
		})
	}
}

func TestOnlyUsers(t *testing.T) {
	m, now, killed := testMonitor(t)
	cfg := enforceCfg()
	cfg.OnlyUsers = []string{"ALICE"}
	root := proc(700, "rootjob", 100)
	root.UID = 0
	run(m, now, cfg, time.Minute, proc(701, "alicejob", 100), root)
	if len(*killed) != 1 || (*killed)[0] != 701 {
		t.Fatalf("only alice's process may be killed, got %v", *killed)
	}
}

func TestNotRootOnlyOwnProcesses(t *testing.T) {
	m, now, killed := testMonitor(t)
	m.elevated, m.selfUID = false, 1001
	other := proc(800, "otherjob", 100)
	other.UID = 0
	run(m, now, enforceCfg(), time.Minute, proc(801, "myjob", 100), other)
	want := []int{801}
	if !unixLike {
		want = []int{800, 801} // Windows has no UID rule
	}
	if len(*killed) != len(want) {
		t.Fatalf("want %v killed, got %v", want, *killed)
	}
}

func TestRestartLoopBackoff(t *testing.T) {
	m, now, killed := testMonitor(t)
	cfg := enforceCfg()
	for i := 0; i < 5; i++ {
		// The service manager restarts it with a new PID each time.
		run(m, now, cfg, time.Minute, proc(900+i, "flappy", 100))
	}
	if len(*killed) != backoffKills {
		t.Fatalf("want %d kills then backoff, got %d (%v)", backoffKills, len(*killed), *killed)
	}
	_, events := m.Snapshot()
	if events[0].Action != "backoff" {
		t.Fatalf("latest event should be backoff, got %+v", events[0])
	}
	*now = now.Add(backoffWindow)
	run(m, now, cfg, time.Minute, proc(950, "flappy", 100))
	if len(*killed) != backoffKills+1 {
		t.Fatalf("after the window it may be killed again, got %v", *killed)
	}
}

func TestKillFailureIsRecorded(t *testing.T) {
	m, now, _ := testMonitor(t)
	m.kill = func(*HotProcess, time.Duration) error { return errors.New("access denied") }
	run(m, now, enforceCfg(), time.Minute, proc(500, "stuck", 100))
	_, events := m.Snapshot()
	if len(events) != 1 || events[0].Action != "kill_failed" || events[0].Error != "access denied" {
		t.Fatalf("got %+v", events)
	}
}

func TestIsDescendantIgnoresReusedParentPIDs(t *testing.T) {
	parents := map[int]parentInfo{
		50:   {PPID: 1000, Start: 100}, // claims hotfixd (PID 1000) as parent…
		1000: {PPID: 1, Start: 500},    // …but hotfixd started later: a reused PID
	}
	if isDescendant(50, 1000, parents) {
		t.Error("a process older than its 'parent' isn't really its child")
	}
	parents[1000] = parentInfo{PPID: 1, Start: 50}
	if !isDescendant(50, 1000, parents) {
		t.Error("real child not detected")
	}
}

func TestUserIn(t *testing.T) {
	if !userIn(`CORP\svc-sql`, []string{"svc-sql"}) || !userIn(`CORP\svc-sql`, []string{`corp\SVC-SQL`}) {
		t.Error("DOMAIN\\name must match by short or full name, case-insensitively")
	}
	if userIn("", []string{""}) || userIn("bob", []string{"alice"}) {
		t.Error("false positive")
	}
}

func TestCPUUsage(t *testing.T) {
	prev := map[int]rawProc{
		1: {PID: 1, StartTime: 1, CPUTime: time.Second},
		2: {PID: 2, StartTime: 5, CPUTime: time.Second},
	}
	cur := map[int]rawProc{
		1: {PID: 1, StartTime: 1, CPUTime: 3 * time.Second}, // 2s of CPU in 1s = 200%
		2: {PID: 2, StartTime: 9, CPUTime: 5 * time.Second}, // reused PID: no baseline
		3: {PID: 3, StartTime: 1, CPUTime: time.Second},     // new: no baseline
	}
	got := cpuUsage(prev, cur, time.Second)
	if len(got) != 1 || got[0].PID != 1 || got[0].CPU < 199.9 || got[0].CPU > 200.1 {
		t.Fatalf("got %+v", got)
	}
}
