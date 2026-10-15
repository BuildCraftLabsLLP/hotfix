//go:build windows

package main

import "os"

// Windows has no reload signal; the config file watcher picks up changes.
func reloadSignals() []os.Signal      { return nil }
func isReloadSignal(s os.Signal) bool { return false }
