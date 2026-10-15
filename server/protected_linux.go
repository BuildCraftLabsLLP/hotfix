//go:build linux

package main

// commMax is the kernel's task-name limit (TASK_COMM_LEN - 1): longer names
// appear truncated in /proc/<pid>/stat.
const commMax = 15

const exeSuffix = ""

// builtinProtected are never killed, whatever the config says. Matched
// case-insensitively against both the task name and the executable name.
var builtinProtected = []string{
	// init and core system services
	"systemd", "init", "kthreadd", "dbus-daemon", "dbus-broker", "dbus-broker-launch",
	"sshd", "sshd-session", "login", "agetty", "getty", "polkitd", "sssd", "nscd",
	"rsyslogd", "syslog-ng", "auditd", "chronyd", "ntpd", "timesyncd",
	"cron", "crond", "atd", "anacron", "irqbalance", "multipathd", "mdadm",
	"lvmetad", "udevd", "firewalld", "NetworkManager", "wpa_supplicant", "dhclient",
	"dnsmasq", "unbound", "haveged", "rngd", "smartd", "acpid", "tuned",
	// package managers: killing one mid-transaction can corrupt the system
	"apt", "apt-get", "aptitude", "dpkg", "unattended-upgrade", "unattended-upgr",
	"packagekitd", "dnf", "dnf-automatic", "yum", "rpm", "zypper", "pacman",
	"snapd", "flatpak", "apk",
	// containers and orchestration
	"containerd", "dockerd", "docker-proxy", "runc", "crun", "conmon", "crio",
	"kubelet", "kube-proxy", "podman", "lxd", "lxcfs",
	// desktop sessions (some servers run one)
	"Xorg", "Xwayland", "gnome-shell", "gdm", "gdm3", "sddm", "lightdm",
	"pipewire", "wireplumber", "pulseaudio",
	// Hotfix itself
	"hotfixd",
}

// protectedPrefixes protect whole families (systemd-journald, systemd-logind,
// containerd-shim-runc-v2, …).
var protectedPrefixes = []string{"systemd-", "containerd-shim", "kworker", "ksoftirqd", "migration/"}
