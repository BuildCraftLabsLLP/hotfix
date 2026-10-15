//go:build linux

package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// procSample is one reading of /proc/<pid>/stat.
type procSample struct {
	PID       int
	PPID      int
	Comm      string // kernel task name, truncated to 15 chars
	Ticks     uint64 // utime + stime, in clock ticks (all threads)
	StartTime uint64 // clock ticks since boot; with PID, uniquely identifies a process
	UID       uint32 // owner of /proc/<pid>
}

// processEntry is a process with its CPU usage over the last poll interval.
type processEntry struct {
	PID       int
	PPID      int
	Name      string  // comm
	CPU       float64 // percent of one core (can exceed 100 for multi-threaded work)
	UID       uint32
	StartTime uint64
}

// parseProcStat parses the contents of /proc/<pid>/stat:
//
//	pid (comm) state ppid pgrp … utime stime … starttime …
//
// comm may itself contain spaces and parentheses, so it is delimited by the
// first '(' and the LAST ')'.
func parseProcStat(data string) (procSample, bool) {
	open := strings.IndexByte(data, '(')
	end := strings.LastIndexByte(data, ')')
	if open < 0 || end < open {
		return procSample{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(data[:open]))
	if err != nil {
		return procSample{}, false
	}

	// Fields after comm, 0-based: state=0 ppid=1 … utime=11 stime=12 … starttime=19
	// (stat fields 3, 4, 14, 15 and 22 in proc(5)).
	rest := strings.Fields(data[end+1:])
	if len(rest) < 20 {
		return procSample{}, false
	}
	ppid, err1 := strconv.Atoi(rest[1])
	utime, err2 := strconv.ParseUint(rest[11], 10, 64)
	stime, err3 := strconv.ParseUint(rest[12], 10, 64)
	start, err4 := strconv.ParseUint(rest[19], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return procSample{}, false
	}

	return procSample{
		PID:       pid,
		PPID:      ppid,
		Comm:      data[open+1 : end],
		Ticks:     utime + stime,
		StartTime: start,
	}, true
}

// readProcSamples takes one reading of every process in /proc. Processes that
// exit mid-scan are skipped.
func readProcSamples() (map[int]procSample, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make(map[int]procSample, len(ents))
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		s, ok := parseProcStat(string(data))
		if !ok {
			continue
		}
		if info, err := e.Info(); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				s.UID = st.Uid
			}
		}
		out[s.PID] = s
	}
	return out, nil
}

// cpuUsage turns two readings taken elapsed seconds apart into per-process CPU
// percentages. Processes that are new (or whose PID was reused) since prev have
// no baseline and are omitted until the next poll.
func cpuUsage(prev, cur map[int]procSample, elapsed float64, ticksPerSec int) []processEntry {
	if elapsed <= 0 || ticksPerSec <= 0 {
		return nil
	}
	entries := make([]processEntry, 0, len(cur))
	for pid, c := range cur {
		p, ok := prev[pid]
		if !ok || p.StartTime != c.StartTime || c.Ticks < p.Ticks {
			continue
		}
		cpu := float64(c.Ticks-p.Ticks) / float64(ticksPerSec) / elapsed * 100
		entries = append(entries, processEntry{
			PID:       pid,
			PPID:      c.PPID,
			Name:      c.Comm,
			CPU:       cpu,
			UID:       c.UID,
			StartTime: c.StartTime,
		})
	}
	return entries
}

var (
	sampleMu     sync.Mutex
	prevSamples  map[int]procSample
	prevSampleAt time.Time
)

// queryProcesses reads /proc and returns per-process CPU usage since the
// previous call. The first call after resetSamples only records a baseline and
// returns no entries.
func queryProcesses() ([]processEntry, error) {
	cur, err := readProcSamples()
	if err != nil {
		return nil, err
	}
	now := time.Now()

	sampleMu.Lock()
	prev, prevAt := prevSamples, prevSampleAt
	prevSamples, prevSampleAt = cur, now
	sampleMu.Unlock()

	if prev == nil {
		return nil, nil
	}
	return cpuUsage(prev, cur, now.Sub(prevAt).Seconds(), clockTicks()), nil
}

// resetSamples drops the CPU baseline (monitor stopped, or the system resumed
// from suspend and the old interval is meaningless).
func resetSamples() {
	sampleMu.Lock()
	prevSamples = nil
	sampleMu.Unlock()
}

// processStartTime returns the start time of pid, or 0 if it no longer exists.
func processStartTime(pid int) uint64 {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s, ok := parseProcStat(string(data))
	if !ok {
		return 0
	}
	return s.StartTime
}

// exeBaseName returns the file name of pid's executable, or "" if unreadable
// (other users' processes, kernel threads, exited processes).
func exeBaseName(pid int) string {
	target, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return ""
	}
	// The kernel appends this when the binary was replaced on disk (e.g. upgraded).
	target = strings.TrimSuffix(target, " (deleted)")
	return filepath.Base(target)
}

var (
	clockTicksOnce sync.Once
	clockTicksVal  = 100
)

// clockTicks returns the kernel's USER_HZ (the unit of utime/stime). It is read
// from the ELF auxiliary vector (AT_CLKTCK) because sysconf needs CGO; it is 100
// on every mainstream Linux, which is also the fallback.
func clockTicks() int {
	clockTicksOnce.Do(func() {
		data, err := os.ReadFile("/proc/self/auxv")
		if err != nil {
			return
		}
		if v := parseAuxvClockTicks(data, strconv.IntSize/8); v > 0 {
			clockTicksVal = v
		}
	})
	return clockTicksVal
}

// parseAuxvClockTicks scans (key, value) pairs of native words for AT_CLKTCK.
func parseAuxvClockTicks(data []byte, wordSize int) int {
	const atClkTck = 17
	word := func(b []byte) uint64 {
		if wordSize == 4 {
			return uint64(binary.NativeEndian.Uint32(b))
		}
		return binary.NativeEndian.Uint64(b)
	}
	for i := 0; i+2*wordSize <= len(data); i += 2 * wordSize {
		if word(data[i:]) == atClkTck {
			return int(word(data[i+wordSize:]))
		}
	}
	return 0
}
