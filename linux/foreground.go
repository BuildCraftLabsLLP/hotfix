//go:build linux

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Linux has no single "which app is focused" API, so this is best-effort and
// shells out to whatever the session provides:
//
//   - Hyprland:      hyprctl activewindow -j
//   - sway:          swaymsg -t get_tree
//   - X11 / XWayland: xdotool, falling back to xprop
//
// GNOME and KDE Wayland sessions expose no such query to ordinary clients; there
// foregroundPID only sees XWayland windows and otherwise returns 0 (no app is
// spared). Users on those sessions should whitelist the apps they care about.

var foregroundWarnOnce sync.Once

// foregroundPID returns the PID that owns the focused window, or 0 if it cannot
// be determined. Safe to call from the monitor goroutine.
func foregroundPID() int {
	if os.Getenv("HYPRLAND_INSTANCE_SIGNATURE") != "" {
		if out, ok := runQuery("hyprctl", "activewindow", "-j"); ok {
			if pid := parseHyprlandPID(out); pid > 0 {
				return pid
			}
		}
	}
	if os.Getenv("SWAYSOCK") != "" {
		if out, ok := runQuery("swaymsg", "-t", "get_tree"); ok {
			if pid := parseSwayFocusedPID(out); pid > 0 {
				return pid
			}
		}
	}
	if os.Getenv("DISPLAY") != "" {
		if out, ok := runQuery("xdotool", "getactivewindow", "getwindowpid"); ok {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && pid > 0 {
				return pid
			}
		}
		if out, ok := runQuery("xprop", "-root", "_NET_ACTIVE_WINDOW"); ok {
			if id := parseXpropWindowID(string(out)); id != "" {
				if out, ok := runQuery("xprop", "-id", id, "_NET_WM_PID"); ok {
					if pid := parseXpropPID(string(out)); pid > 0 {
						return pid
					}
				}
			}
		}
	}

	foregroundWarnOnce.Do(func() {
		logf("monitor: cannot determine the focused app on this session — " +
			"\"Protect Active App\" has no effect (see README: Linux)")
	})
	return 0
}

// runQuery runs a short helper command with a timeout. ok is false if the tool
// is missing, fails, or hangs.
func runQuery(name string, args ...string) ([]byte, bool) {
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).Output()
	if err != nil {
		return nil, false
	}
	return out, true
}

var (
	xpropWindowRe = regexp.MustCompile(`window id # (0x[0-9a-fA-F]+)`)
	xpropPIDRe    = regexp.MustCompile(`_NET_WM_PID\(CARDINAL\) = (\d+)`)
)

// parseXpropWindowID extracts the window id from
// `_NET_ACTIVE_WINDOW(WINDOW): window id # 0x3a00007`. "0x0" (no focused X
// window, e.g. a native Wayland app has focus) yields "".
func parseXpropWindowID(out string) string {
	m := xpropWindowRe.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	if n, err := strconv.ParseUint(m[1], 0, 64); err != nil || n == 0 {
		return ""
	}
	return m[1]
}

// parseXpropPID extracts the PID from `_NET_WM_PID(CARDINAL) = 4242`.
func parseXpropPID(out string) int {
	m := xpropPIDRe.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	pid, _ := strconv.Atoi(m[1])
	return pid
}

// parseHyprlandPID reads the "pid" field of `hyprctl activewindow -j`.
func parseHyprlandPID(out []byte) int {
	var w struct {
		PID int `json:"pid"`
	}
	if json.Unmarshal(out, &w) != nil {
		return 0
	}
	return w.PID
}

// swayNode is the subset of sway's get_tree layout we need.
type swayNode struct {
	Focused       bool       `json:"focused"`
	PID           int        `json:"pid"`
	Nodes         []swayNode `json:"nodes"`
	FloatingNodes []swayNode `json:"floating_nodes"`
}

// parseSwayFocusedPID walks `swaymsg -t get_tree` for the focused window's PID.
func parseSwayFocusedPID(out []byte) int {
	var root swayNode
	if json.Unmarshal(out, &root) != nil {
		return 0
	}
	return root.focusedPID()
}

func (n *swayNode) focusedPID() int {
	if n.Focused && n.PID > 0 {
		return n.PID
	}
	for i := range n.Nodes {
		if pid := n.Nodes[i].focusedPID(); pid > 0 {
			return pid
		}
	}
	for i := range n.FloatingNodes {
		if pid := n.FloatingNodes[i].focusedPID(); pid > 0 {
			return pid
		}
	}
	return 0
}
