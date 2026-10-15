package main

import "time"

// rawProc is one reading of one process, as each OS's sampler takes it.
type rawProc struct {
	PID          int
	PPID         int
	Name         string        // short process name (Linux comm, macOS p_comm, Windows exe file name)
	UID          uint32        // owner (Linux/macOS; 0 on Windows)
	StartTime    uint64        // opaque per-OS start stamp; with PID it identifies one process
	CPUTime      time.Duration // user + system CPU time used so far, all threads
	KernelThread bool          // never a candidate
}

// Process is a process with its CPU usage over the last sampling interval.
type Process struct {
	PID          int
	PPID         int
	Name         string
	UID          uint32
	StartTime    uint64
	CPU          float64 // percent of ONE core (like top; can exceed 100 for multi-threaded work)
	KernelThread bool
}

// procKey identifies one process across samples: PIDs get reused, so the start
// time is part of the identity.
type procKey struct {
	PID   int
	Start uint64
}

func (p Process) key() procKey { return procKey{p.PID, p.StartTime} }

// cpuUsage turns two readings taken elapsed apart into per-process CPU
// percentages. Processes that are new since prev (or whose PID was reused) have
// no baseline and are left out until the next sample.
//
// Adapted from linux/procstat.go cpuUsage, with CPU time in a common unit so
// every OS shares it.
func cpuUsage(prev, cur map[int]rawProc, elapsed time.Duration) []Process {
	if elapsed <= 0 {
		return nil
	}
	out := make([]Process, 0, len(cur))
	for pid, c := range cur {
		p, ok := prev[pid]
		if !ok || p.StartTime != c.StartTime || c.CPUTime < p.CPUTime {
			continue
		}
		out = append(out, Process{
			PID:          pid,
			PPID:         c.PPID,
			Name:         c.Name,
			UID:          c.UID,
			StartTime:    c.StartTime,
			CPU:          float64(c.CPUTime-p.CPUTime) / float64(elapsed) * 100,
			KernelThread: c.KernelThread,
		})
	}
	return out
}

// sampler keeps the previous reading so each call returns usage since the last.
type sampler struct {
	read   func() (map[int]rawProc, error) // the OS-specific reader
	prev   map[int]rawProc
	prevAt time.Time
	now    func() time.Time
}

func newSampler() *sampler {
	return &sampler{read: readProcesses, now: time.Now}
}

// Sample returns CPU usage since the previous call. The first call (and the
// first after Reset) only records a baseline and returns nothing.
func (s *sampler) Sample() ([]Process, error) {
	cur, err := s.read()
	if err != nil {
		return nil, err
	}
	now := s.now()
	prev, prevAt := s.prev, s.prevAt
	s.prev, s.prevAt = cur, now
	if prev == nil {
		return nil, nil
	}
	return cpuUsage(prev, cur, now.Sub(prevAt)), nil
}

// Reset drops the baseline (e.g. after a long pause, when the old interval is meaningless).
func (s *sampler) Reset() { s.prev = nil }
