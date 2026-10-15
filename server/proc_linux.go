//go:build linux

package main

// Process reading adapted from linux/procstat.go (parseProcStat, clockTicks,
// parseAuxvClockTicks), plus kernel-thread detection via PF_KTHREAD and a
// pidfd-based kill.

import (
	"encoding/binary"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// PIDs 1 (init) and 2 (kthreadd) are never candidates.
const maxReservedPID = 2

// pfKthread is PF_KTHREAD in the stat flags field.
const pfKthread = 0x00200000

type procStat struct {
	PID, PPID int
	Comm      string
	Flags     uint64
	Ticks     uint64 // utime + stime
	StartTime uint64 // clock ticks since boot
}

// parseProcStat parses /proc/<pid>/stat. comm may contain spaces and
// parentheses, so it is delimited by the first '(' and the LAST ')'.
func parseProcStat(data string) (procStat, bool) {
	open := strings.IndexByte(data, '(')
	end := strings.LastIndexByte(data, ')')
	if open < 0 || end < open {
		return procStat{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(data[:open]))
	if err != nil {
		return procStat{}, false
	}
	// Fields after comm, 0-based: state=0 ppid=1 … flags=6 … utime=11 stime=12 … starttime=19.
	rest := strings.Fields(data[end+1:])
	if len(rest) < 20 {
		return procStat{}, false
	}
	ppid, e1 := strconv.Atoi(rest[1])
	flags, e2 := strconv.ParseUint(rest[6], 10, 64)
	utime, e3 := strconv.ParseUint(rest[11], 10, 64)
	stime, e4 := strconv.ParseUint(rest[12], 10, 64)
	start, e5 := strconv.ParseUint(rest[19], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil {
		return procStat{}, false
	}
	return procStat{PID: pid, PPID: ppid, Comm: data[open+1 : end], Flags: flags, Ticks: utime + stime, StartTime: start}, true
}

func readProcesses() (map[int]rawProc, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	hz := time.Duration(clockTicks())
	out := make(map[int]rawProc, len(ents))
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue // exited mid-scan
		}
		s, ok := parseProcStat(string(data))
		if !ok {
			continue
		}
		r := rawProc{
			PID: s.PID, PPID: s.PPID, Name: s.Comm, StartTime: s.StartTime,
			CPUTime:      time.Duration(s.Ticks) * time.Second / hz,
			KernelThread: s.Flags&pfKthread != 0 || s.PID == 2,
		}
		if info, err := e.Info(); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				r.UID = st.Uid
			}
		}
		out[r.PID] = r
	}
	return out, nil
}

// processStart returns pid's start time, or 0 if it no longer exists.
func processStart(pid int) uint64 {
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

// exeName returns the file name of pid's executable ("" if unreadable).
func exeName(pid int) string {
	target, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return ""
	}
	return filepath.Base(strings.TrimSuffix(target, " (deleted)"))
}

var (
	userCacheMu sync.Mutex
	userCache   = map[uint32]string{}
)

// processUser returns the owner's user name (the numeric UID if unknown).
func processUser(p Process) string {
	userCacheMu.Lock()
	defer userCacheMu.Unlock()
	if n, ok := userCache[p.UID]; ok {
		return n
	}
	id := strconv.FormatUint(uint64(p.UID), 10)
	n := id
	if u, err := user.LookupId(id); err == nil {
		n = u.Username
	}
	userCache[p.UID] = n
	return n
}

// containerMarkers in /proc/<pid>/cgroup mean the process runs in a container.
var containerMarkers = []string{"docker", "containerd", "kubepods", "libpod", "crio", "lxc", "machine.slice"}

func inContainer(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return false
	}
	s := string(data)
	for _, m := range containerMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// selfInContainer reports whether hotfixd itself runs in a container, where it
// can only see the container's own processes.
func selfInContainer() bool {
	for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return inContainer(1) && !strings.Contains(readFirstLine("/proc/1/comm"), "systemd")
}

func readFirstLine(path string) string {
	data, _ := os.ReadFile(path)
	line, _, _ := strings.Cut(string(data), "\n")
	return line
}

// killProcess sends SIGTERM and, if the process is still alive after grace,
// SIGKILL. It signals through a pidfd opened after checking the start time, so
// the signals can only ever reach that exact process even if its PID is reused.
func killProcess(h *HotProcess, grace time.Duration) error {
	fd, err := unix.PidfdOpen(h.PID, 0)
	if errors.Is(err, unix.ENOSYS) {
		return killByPID(h, grace) // kernel older than 5.3
	}
	if err != nil {
		return err
	}
	if processStart(h.PID) != h.StartTime {
		unix.Close(fd)
		return errors.New("process already exited")
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0); err != nil {
		unix.Close(fd)
		return err
	}
	go func() {
		defer unix.Close(fd)
		if !waitPidfd(fd, grace) {
			logf("monitor: %s (PID %d) ignored SIGTERM; sending SIGKILL", h.DisplayName(), h.PID)
			_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		}
	}()
	return nil
}

// waitPidfd reports whether the process exited within d (a pidfd becomes
// readable when its process exits).
func waitPidfd(fd int, d time.Duration) bool {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	deadline := time.Now().Add(d)
	for {
		left := time.Until(deadline)
		if left < 0 {
			left = 0
		}
		n, err := unix.Poll(fds, int(left/time.Millisecond))
		if err == unix.EINTR {
			continue
		}
		return err == nil && n > 0
	}
}

// killByPID is the fallback without pidfds: SIGTERM, then SIGKILL only if the
// same PID still has the same start time.
func killByPID(h *HotProcess, grace time.Duration) error {
	if processStart(h.PID) != h.StartTime {
		return errors.New("process already exited")
	}
	if err := syscall.Kill(h.PID, syscall.SIGTERM); err != nil {
		return err
	}
	time.AfterFunc(grace, func() {
		if processStart(h.PID) == h.StartTime {
			logf("monitor: %s (PID %d) ignored SIGTERM; sending SIGKILL", h.DisplayName(), h.PID)
			_ = syscall.Kill(h.PID, syscall.SIGKILL)
		}
	})
	return nil
}

var (
	clockTicksOnce sync.Once
	clockTicksVal  = 100
)

// clockTicks returns USER_HZ, read from the ELF auxiliary vector (AT_CLKTCK)
// because sysconf needs CGO. 100 on every mainstream Linux, also the fallback.
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
