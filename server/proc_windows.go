//go:build windows

package main

// Process reading adapted from windows/apps.go (Toolhelp snapshot +
// GetProcessTimes), WITHOUT its same-session filter: the service runs in
// session 0 and must see every session. Pure Go: no PowerShell, no taskkill.

import (
	"errors"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// PIDs 0 (Idle) and 4 (System) are never candidates.
const maxReservedPID = 4

func ftUint64(t windows.Filetime) uint64 {
	return uint64(t.HighDateTime)<<32 | uint64(t.LowDateTime)
}

// processTimes returns a process's creation time and total CPU time.
func processTimes(h windows.Handle) (created uint64, cpu time.Duration, err error) {
	var c, exited, kernel, user windows.Filetime
	if err = windows.GetProcessTimes(h, &c, &exited, &kernel, &user); err != nil {
		return 0, 0, err
	}
	// FILETIME durations are in 100 ns units.
	return ftUint64(c), time.Duration(ftUint64(kernel)+ftUint64(user)) * 100, nil
}

func readProcesses() (map[int]rawProc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)

	out := map[int]rawProc{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		pid := int(e.ProcessID)
		r := rawProc{PID: pid, PPID: int(e.ParentProcessID), Name: windows.UTF16ToString(e.ExeFile[:])}
		if pid <= maxReservedPID {
			r.KernelThread = true
			out[pid] = r
			continue
		}
		// Protected processes (PPL) may refuse even limited access; they are
		// skipped (they're on the protected list anyway).
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
		if err != nil {
			continue
		}
		r.StartTime, r.CPUTime, err = processTimes(h)
		windows.CloseHandle(h)
		if err != nil {
			continue
		}
		out[pid] = r
	}
	return out, nil
}

// exeName: Toolhelp names are already the executable's file name.
func exeName(pid int) string { return "" }

var (
	userCacheMu sync.Mutex
	userCache   = map[procKey]string{}
)

// processUser returns the process owner as DOMAIN\name ("" if unreadable).
func processUser(p Process) string {
	userCacheMu.Lock()
	if n, ok := userCache[p.key()]; ok {
		userCacheMu.Unlock()
		return n
	}
	userCacheMu.Unlock()

	n := lookupProcessUser(uint32(p.PID))
	userCacheMu.Lock()
	if len(userCache) > 4096 {
		userCache = map[procKey]string{}
	}
	userCache[p.key()] = n
	userCacheMu.Unlock()
	return n
}

func lookupProcessUser(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return ""
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return ""
	}
	account, domain, _, err := tu.User.Sid.LookupAccount("")
	if err != nil {
		return tu.User.Sid.String()
	}
	if domain != "" {
		return domain + `\` + account
	}
	return account
}

func inContainer(pid int) bool { return false }

func selfInContainer() bool { return false }

// killProcess terminates the process through one handle: the creation time is
// checked on the same handle that's used to terminate, and an open handle
// keeps its PID from being reused, so only that exact process can be killed.
// Windows has no SIGTERM for services/background processes, so grace is unused.
func killProcess(h *HotProcess, _ time.Duration) error {
	ph, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(h.PID))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(ph)
	created, _, err := processTimes(ph)
	if err != nil {
		return err
	}
	if created != h.StartTime {
		return errors.New("process already exited")
	}
	return windows.TerminateProcess(ph, 1)
}
