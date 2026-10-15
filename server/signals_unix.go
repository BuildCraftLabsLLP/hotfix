//go:build linux || darwin

package main

import (
	"os"
	"syscall"
)

// SIGHUP rereads the config (`systemctl reload hotfixd`).
func reloadSignals() []os.Signal      { return []os.Signal{syscall.SIGHUP} }
func isReloadSignal(s os.Signal) bool { return s == syscall.SIGHUP }
