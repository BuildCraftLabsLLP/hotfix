//go:build windows

package main

// Windows process names are full executable names; nothing is truncated.
const commMax = 0

const exeSuffix = ".exe"

// builtinProtected are never killed, whatever the config says. Matched
// case-insensitively, with or without ".exe".
var builtinProtected = []string{
	// kernel and session infrastructure
	"System", "Idle", "Registry", "Secure System", "Memory Compression", "MemCompression",
	"smss", "csrss", "wininit", "winlogon", "services", "lsass", "LsaIso",
	"svchost", "fontdrvhost", "dwm", "sihost", "WUDFHost", "spoolsv", "wmiprvse",
	// security and updates
	"MsMpEng", "NisSrv", "MpDefenderCoreService", "SecurityHealthService",
	"TrustedInstaller", "TiWorker", "wuauclt", "UsoClient", "MoUsoCoreWorker",
	// virtualization and containers
	"vmms", "vmwp", "vmmem", "vmcompute", "dockerd", "containerd",
	// remote access
	"sshd", "TermService",
	// Hotfix itself
	"hotfixd",
}

var protectedPrefixes []string
