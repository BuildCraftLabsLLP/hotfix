//go:build linux

package main

import (
	"syscall"

	"github.com/godbus/dbus/v5"
)

const (
	login1Dest      = "org.freedesktop.login1"
	login1Path      = "/org/freedesktop/login1"
	login1Manager   = "org.freedesktop.login1.Manager"
	prepareForSleep = login1Manager + ".PrepareForSleep"
)

// watchSleep starts a goroutine that listens for systemd-logind's
// PrepareForSleep signal on the system bus and calls killAllHot before the
// machine suspends when KillOnSleep is enabled.
//
// It holds a "delay" inhibitor lock so logind waits for the kills to be sent
// before suspending; the lock is released as soon as we're done (and always on
// PrepareForSleep, so Hotfix never holds up suspend) and re-taken on resume.
// Without logind (non-systemd distros) this logs once and does nothing.
func watchSleep() {
	safeGo("sleep-watcher", func() {
		conn, err := dbus.ConnectSystemBus()
		if err != nil {
			logf("sleep-watcher: system bus unavailable: %v", err)
			return
		}
		defer conn.Close()

		if err := conn.AddMatchSignal(
			dbus.WithMatchObjectPath(login1Path),
			dbus.WithMatchInterface(login1Manager),
			dbus.WithMatchMember("PrepareForSleep"),
		); err != nil {
			logf("sleep-watcher: subscribe error: %v", err)
			return
		}

		ch := make(chan *dbus.Signal, 4)
		conn.Signal(ch)

		login := conn.Object(login1Dest, login1Path)
		lock := takeSleepDelay(login)

		for sig := range ch {
			if sig.Name != prepareForSleep || len(sig.Body) == 0 {
				continue
			}
			sleeping, _ := sig.Body[0].(bool)
			if sleeping {
				cfg := getConfig()
				if cfg.Enabled && cfg.KillOnSleep {
					logf("sleep-watcher: suspend detected — killing hot processes")
					killAllHot()
				}
				releaseSleepDelay(lock)
				lock = -1
			} else {
				// Time spent suspended must not count toward "hot for N seconds".
				resetHot()
				lock = takeSleepDelay(login)
			}
		}
		logf("sleep-watcher: exited")
	})
}

// takeSleepDelay asks logind for a delay inhibitor and returns its fd, or -1.
func takeSleepDelay(login dbus.BusObject) int {
	var fd dbus.UnixFD
	err := login.Call(login1Manager+".Inhibit", 0,
		"sleep", "Hotfix", "Stop runaway processes before suspend", "delay").Store(&fd)
	if err != nil {
		logf("sleep-watcher: inhibitor unavailable: %v", err)
		return -1
	}
	return int(fd)
}

func releaseSleepDelay(fd int) {
	if fd >= 0 {
		_ = syscall.Close(fd)
	}
}
