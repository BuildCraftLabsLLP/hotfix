//go:build darwin

package main

// p_comm holds up to MAXCOMLEN (16) characters.
const commMax = 16

const exeSuffix = ""

// builtinProtected are never killed, whatever the config says. Matched
// case-insensitively against both p_comm and the executable's file name.
var builtinProtected = []string{
	"launchd", "kernel_task", "WindowServer", "loginwindow", "logd", "notifyd",
	"configd", "opendirectoryd", "securityd", "trustd", "syspolicyd", "powerd",
	"fseventsd", "diskarbitrationd", "coreaudiod", "cfprefsd", "distnoted",
	"UserEventAgent", "hidd", "kextd", "apsd", "mDNSResponder", "sshd", "sshd-session",
	// Spotlight, backups and updates
	"mds", "mds_stores", "mdworker", "mdworker_shared", "backupd", "softwareupdated",
	"installd", "system_installd",
	// GUI shell (some Mac build servers keep a login session)
	"Finder", "Dock", "SystemUIServer",
	// Hotfix itself
	"hotfixd",
}

var protectedPrefixes = []string{"com.apple."}
