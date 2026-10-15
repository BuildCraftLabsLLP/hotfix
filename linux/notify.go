//go:build linux

package main

import (
	"fmt"

	"github.com/godbus/dbus/v5"
)

// notifyToast shows a desktop notification through the freedesktop
// Notifications service on the session bus (what notify-send talks to), so no
// external binary is needed. It never blocks the caller.
func notifyToast(title, body string) {
	go func() {
		conn, err := dbus.SessionBus()
		if err != nil {
			logf("notify: session bus unavailable: %v", err)
			return
		}
		obj := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
		call := obj.Call("org.freedesktop.Notifications.Notify", 0,
			"Hotfix",                  // app_name
			uint32(0),                 // replaces_id
			"hotfix",                  // app_icon (installed into the icon theme by install.sh)
			title,                     // summary
			body,                      // body
			[]string{},                // actions
			map[string]dbus.Variant{}, // hints
			int32(-1),                 // expire_timeout: server default
		)
		if call.Err != nil {
			logf("notify: notification failed: %v", call.Err)
		}
	}()
}

// notifyKilledToast is the desktop-notification counterpart to the tray label
// update in notifyKilled. Kept separate so callers can choose either or both.
func notifyKilledToast(name string, pid int, cpu float64) {
	title := "Hotfix — Process Terminated"
	body := fmt.Sprintf("%s (PID %d) was using %.0f%% CPU and has been terminated.", name, pid, cpu)
	notifyToast(title, body)
}
