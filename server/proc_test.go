package main

// Live tests of each OS's process reading and killing, using copies of this
// test binary as child processes (HOTFIXD_TEST_CHILD=busy spins a core,
// =sleep idles).

import (
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	switch os.Getenv("HOTFIXD_TEST_CHILD") {
	case "busy":
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
		}
		os.Exit(0)
	case "sleep":
		time.Sleep(60 * time.Second)
		os.Exit(0)
	}
	logStdout = io.Discard
	os.Exit(m.Run())
}

func startChild(t *testing.T, kind string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), "HOTFIXD_TEST_CHILD="+kind)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	return cmd
}

func TestReadProcessesSeesSelfAndSystem(t *testing.T) {
	procs, err := readProcesses()
	if err != nil {
		t.Fatal(err)
	}
	self, ok := procs[os.Getpid()]
	if !ok {
		t.Fatal("own process not listed")
	}
	if self.StartTime == 0 || self.Name == "" {
		t.Errorf("own entry incomplete: %+v", self)
	}
	if self.PPID != os.Getppid() {
		t.Errorf("PPID %d, want %d", self.PPID, os.Getppid())
	}
	if len(procs) < 5 {
		t.Errorf("only %d processes listed", len(procs))
	}
}

func TestBusyChildMeasuredAtAboutOneCore(t *testing.T) {
	child := startChild(t, "busy")
	s := newSampler()
	time.Sleep(300 * time.Millisecond)
	s.Sample()
	time.Sleep(2 * time.Second)
	procs, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		if p.PID == child.Process.Pid {
			// One spinning thread ≈ 100% of a core; CI machines are noisy.
			if p.CPU < 40 || p.CPU > 160 {
				t.Errorf("busy child measured at %.1f%%, want about 100%%", p.CPU)
			}
			return
		}
	}
	t.Fatal("busy child not in the sample")
}

func TestKillProcessKillsTheRightProcess(t *testing.T) {
	child := startChild(t, "sleep")
	time.Sleep(300 * time.Millisecond)
	procs, err := readProcesses()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := procs[child.Process.Pid]
	if !ok {
		t.Fatal("child not listed")
	}
	h := &HotProcess{Process: Process{PID: r.PID, Name: r.Name, StartTime: r.StartTime}}

	// A stale start time (the PID now belongs to another process) must be refused.
	stale := *h
	stale.StartTime++
	if killProcess(&stale, time.Second) == nil {
		t.Fatal("killed a process whose start time doesn't match")
	}

	if err := killProcess(h, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("child still alive 10s after killProcess")
	}
}

func TestProtectedListsMatchWithOSRules(t *testing.T) {
	if !isProtected("hotfixd") || !isProtected("HOTFIXD") {
		t.Error("hotfixd must protect itself, case-insensitively")
	}
	if isProtected("") || isProtected("definitely-not-a-system-process") {
		t.Error("false positive")
	}
	if exeSuffix != "" && !isProtected("hotfixd"+exeSuffix) {
		t.Error("exe suffix not stripped")
	}
}
