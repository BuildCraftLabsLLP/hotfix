package main

import "strings"

// normalizeName lowercases a process name for matching and drops a trailing
// exeSuffix (".exe" on Windows), so "MsMpEng.exe", "msmpeng" and "MsMpEng"
// all match.
func normalizeName(n string) string {
	n = strings.ToLower(strings.TrimSpace(n))
	if exeSuffix != "" {
		n = strings.TrimSuffix(n, exeSuffix)
	}
	return n
}

// nameSet builds a case-insensitive lookup that also holds each name's
// truncation to commMax characters, because the kernel truncates short process
// names (Linux to 15, macOS to 16), so "unattended-upgrade" shows up as
// "unattended-upgr". Adapted from linux/monitor.go nameSet.
func nameSet(names []string) map[string]bool {
	set := make(map[string]bool, 2*len(names))
	for _, n := range names {
		n = normalizeName(n)
		if n == "" {
			continue
		}
		set[n] = true
		if commMax > 0 && len(n) > commMax {
			set[n[:commMax]] = true
		}
	}
	return set
}

var builtinSet = nameSet(builtinProtected)

// isProtected reports whether a process must never be killed, whatever the
// config says: core OS services, package managers mid-transaction, container
// runtimes. The lists are in protected_<os>.go.
func isProtected(name string) bool {
	n := normalizeName(name)
	if n == "" {
		return false
	}
	if builtinSet[n] {
		return true
	}
	for _, p := range protectedPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}
