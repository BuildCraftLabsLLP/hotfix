//go:build darwin

package main

// macOS process reading. The process list (pid, ppid, uid, start time, name)
// comes from sysctl kern.proc.all (pure Go via x/sys); CPU times and the
// executable path come from libproc, called through purego so the build stays
// CGO-free and cross-compiles from any OS. (Not `ps`: its lstart/time output
// depends on locale and timezone.)

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

// PID 0 (kernel_task) and 1 (launchd) are never candidates.
const maxReservedPID = 1

const (
	procPidTaskInfo   = 4    // PROC_PIDTASKINFO
	procPidPathMaxLen = 4096 // PROC_PIDPATHINFO_MAXSIZE
)

// procTaskInfo mirrors struct proc_taskinfo (sys/proc_info.h).
type procTaskInfo struct {
	VirtualSize, ResidentSize      uint64
	TotalUser, TotalSystem         uint64 // Mach absolute time units
	ThreadsUser, ThreadsSystem     uint64
	Policy, Faults, Pageins        int32
	CowFaults, MessagesSent        int32
	MessagesReceived, SyscallsMach int32
	SyscallsUnix, Csw, Threadnum   int32
	NumRunning, Priority           int32
}

type machTimebaseInfo struct{ Numer, Denom uint32 }

var (
	libprocOnce sync.Once
	libprocErr  error

	procPidinfo  func(pid, flavor int32, arg uint64, buf unsafe.Pointer, size int32) int32
	procPidpath  func(pid int32, buf unsafe.Pointer, size uint32) int32
	machTimebase func(info *machTimebaseInfo) int32
	timebase     = machTimebaseInfo{1, 1}
)

func loadLibproc() error {
	libprocOnce.Do(func() {
		lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			libprocErr = err
			return
		}
		purego.RegisterLibFunc(&procPidinfo, lib, "proc_pidinfo")
		purego.RegisterLibFunc(&procPidpath, lib, "proc_pidpath")
		purego.RegisterLibFunc(&machTimebase, lib, "mach_timebase_info")
		var tb machTimebaseInfo
		if machTimebase(&tb) == 0 && tb.Numer != 0 && tb.Denom != 0 {
			timebase = tb // 1/1 on Intel, 125/3 on Apple Silicon
		}
	})
	return libprocErr
}

// machToDuration converts Mach absolute time units to a duration.
func machToDuration(t uint64) time.Duration {
	return time.Duration(t / uint64(timebase.Denom) * uint64(timebase.Numer))
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func startStamp(tv unix.Timeval) uint64 {
	return uint64(tv.Sec)*1_000_000 + uint64(tv.Usec)
}

func readProcesses() (map[int]rawProc, error) {
	if err := loadLibproc(); err != nil {
		return nil, err
	}
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make(map[int]rawProc, len(kps))
	for i := range kps {
		kp := &kps[i]
		pid := int(kp.Proc.P_pid)
		r := rawProc{
			PID:       pid,
			PPID:      int(kp.Eproc.Ppid),
			Name:      cString(kp.Proc.P_comm[:]),
			UID:       kp.Eproc.Ucred.Uid,
			StartTime: startStamp(kp.Proc.P_starttime),
		}
		if pid <= maxReservedPID {
			r.KernelThread = true
			out[pid] = r
			continue
		}
		var ti procTaskInfo
		size := int32(unsafe.Sizeof(ti))
		if procPidinfo(int32(pid), procPidTaskInfo, 0, unsafe.Pointer(&ti), size) != size {
			continue // exited, or not ours to read (without root)
		}
		r.CPUTime = machToDuration(ti.TotalUser + ti.TotalSystem)
		out[pid] = r
	}
	return out, nil
}

// exeName returns the file name of pid's executable (p_comm is cut at 16 chars).
func exeName(pid int) string {
	if loadLibproc() != nil {
		return ""
	}
	buf := make([]byte, procPidPathMaxLen)
	if n := procPidpath(int32(pid), unsafe.Pointer(&buf[0]), uint32(len(buf))); n > 0 {
		return filepath.Base(string(buf[:n]))
	}
	return ""
}

var (
	userCacheMu sync.Mutex
	userCache   = map[uint32]string{}
)

// processUser returns the owner's user name. os/user without cgo only reads
// /etc/passwd, which on macOS lacks directory-service users, so ask `id`.
func processUser(p Process) string {
	userCacheMu.Lock()
	defer userCacheMu.Unlock()
	if n, ok := userCache[p.UID]; ok {
		return n
	}
	id := strconv.FormatUint(uint64(p.UID), 10)
	n := id
	if out, err := exec.Command("/usr/bin/id", "-un", id).Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			n = s
		}
	}
	userCache[p.UID] = n
	return n
}

func inContainer(pid int) bool { return false }

func selfInContainer() bool { return false }

// processStart returns pid's start stamp, or 0 if it no longer exists.
func processStart(pid int) uint64 {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || int(kp.Proc.P_pid) != pid {
		return 0
	}
	return startStamp(kp.Proc.P_starttime)
}

// killProcess sends SIGTERM and, if the same process (PID + start time) is
// still alive after grace, SIGKILL.
func killProcess(h *HotProcess, grace time.Duration) error {
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
