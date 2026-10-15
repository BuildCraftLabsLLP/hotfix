//go:build linux

package main

import (
	"encoding/binary"
	"os"
	"testing"
)

// A realistic /proc/<pid>/stat line (fields: pid, comm, state, ppid, …,
// utime=14th, stime=15th, …, starttime=22nd).
const sampleStat = "4242 (chrome) R 1200 4242 4242 0 -1 4194304 1000 0 0 0 " +
	"7300 250 0 0 20 0 30 0 987654 123456789 5000 18446744073709551615 " +
	"1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0"

func TestParseProcStat_Basic(t *testing.T) {
	s, ok := parseProcStat(sampleStat)
	if !ok {
		t.Fatal("expected sample stat line to parse")
	}
	if s.PID != 4242 || s.PPID != 1200 {
		t.Errorf("pid/ppid: got %d/%d, want 4242/1200", s.PID, s.PPID)
	}
	if s.Comm != "chrome" {
		t.Errorf("comm: got %q, want %q", s.Comm, "chrome")
	}
	if s.Ticks != 7550 {
		t.Errorf("ticks: got %d, want 7550 (utime 7300 + stime 250)", s.Ticks)
	}
	if s.StartTime != 987654 {
		t.Errorf("starttime: got %d, want 987654", s.StartTime)
	}
}

// comm can contain spaces and parentheses; only the LAST ')' ends it.
func TestParseProcStat_CommWithSpacesAndParens(t *testing.T) {
	line := "77 (Web Content (x) y) S 50 77 77 0 -1 0 0 0 0 0 " +
		"10 5 0 0 20 0 1 0 111 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0"
	s, ok := parseProcStat(line)
	if !ok {
		t.Fatal("expected line to parse")
	}
	if s.Comm != "Web Content (x) y" {
		t.Errorf("comm: got %q", s.Comm)
	}
	if s.PPID != 50 || s.Ticks != 15 || s.StartTime != 111 {
		t.Errorf("got ppid=%d ticks=%d start=%d, want 50/15/111", s.PPID, s.Ticks, s.StartTime)
	}
}

func TestParseProcStat_RejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "no parens here", "12 (short) R 1 2 3", "x (comm) R 1"} {
		if _, ok := parseProcStat(in); ok {
			t.Errorf("parseProcStat(%q) should fail", in)
		}
	}
}

func TestCPUUsage_ComputesPercentOfOneCore(t *testing.T) {
	prev := map[int]procSample{10: {PID: 10, Comm: "busy", Ticks: 1000, StartTime: 5}}
	cur := map[int]procSample{10: {PID: 10, Comm: "busy", Ticks: 1450, StartTime: 5, UID: 1000}}

	// 450 ticks at 100 Hz = 4.5 CPU-seconds over 5 s = 90% of one core.
	entries := cpuUsage(prev, cur, 5.0, 100)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if got := entries[0].CPU; got < 89.99 || got > 90.01 {
		t.Errorf("cpu: got %.2f, want 90", got)
	}
	if entries[0].Name != "busy" || entries[0].UID != 1000 {
		t.Errorf("entry fields not carried over: %+v", entries[0])
	}
}

func TestCPUUsage_MultiThreadedExceeds100(t *testing.T) {
	prev := map[int]procSample{10: {PID: 10, Ticks: 0, StartTime: 5}}
	cur := map[int]procSample{10: {PID: 10, Ticks: 2000, StartTime: 5}}
	entries := cpuUsage(prev, cur, 5.0, 100)
	if len(entries) != 1 || entries[0].CPU < 399 {
		t.Fatalf("expected ~400%% for 4 busy threads, got %+v", entries)
	}
}

func TestCPUUsage_SkipsNewAndReusedPIDs(t *testing.T) {
	prev := map[int]procSample{
		10: {PID: 10, Ticks: 1000, StartTime: 5},
	}
	cur := map[int]procSample{
		10: {PID: 10, Ticks: 50, StartTime: 999}, // PID reused by a new process
		11: {PID: 11, Ticks: 500, StartTime: 7},  // brand new, no baseline
	}
	if entries := cpuUsage(prev, cur, 5.0, 100); len(entries) != 0 {
		t.Errorf("expected no entries without a valid baseline, got %+v", entries)
	}
}

func TestCPUUsage_InvalidInterval(t *testing.T) {
	m := map[int]procSample{1: {PID: 1}}
	if e := cpuUsage(m, m, 0, 100); e != nil {
		t.Errorf("zero elapsed should yield nil, got %+v", e)
	}
}

func TestParseAuxvClockTicks(t *testing.T) {
	put := func(b []byte, v uint64) []byte { return binary.NativeEndian.AppendUint64(b, v) }
	var data []byte
	data = put(put(data, 6), 4096) // AT_PAGESZ
	data = put(put(data, 17), 100) // AT_CLKTCK
	data = put(put(data, 0), 0)    // AT_NULL
	if got := parseAuxvClockTicks(data, 8); got != 100 {
		t.Errorf("got %d, want 100", got)
	}
	if got := parseAuxvClockTicks(data[:16], 8); got != 0 {
		t.Errorf("missing AT_CLKTCK should yield 0, got %d", got)
	}
}

// Reads the real /proc: the test process itself must show up, owned by us.
func TestReadProcSamples_FindsSelf(t *testing.T) {
	samples, err := readProcSamples()
	if err != nil {
		t.Fatalf("readProcSamples: %v", err)
	}
	self, ok := samples[os.Getpid()]
	if !ok {
		t.Fatal("own PID missing from /proc scan")
	}
	if self.UID != uint32(os.Getuid()) {
		t.Errorf("uid: got %d, want %d", self.UID, os.Getuid())
	}
	if self.StartTime == 0 {
		t.Error("expected a non-zero start time")
	}
	if got := processStartTime(os.Getpid()); got != self.StartTime {
		t.Errorf("processStartTime: got %d, want %d", got, self.StartTime)
	}
	if exeBaseName(os.Getpid()) == "" {
		t.Error("expected to resolve own executable name")
	}
	if clockTicks() <= 0 {
		t.Error("clockTicks must be positive")
	}
}
