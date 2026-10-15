//go:build linux

package main

import (
	"errors"
	"os"
	"strings"
)

var machineIDPaths = []string{"/etc/machine-id", "/var/lib/dbus/machine-id"}

func machineID() (string, error) {
	for _, p := range machineIDPaths {
		if data, err := os.ReadFile(p); err == nil {
			if id := strings.TrimSpace(string(data)); id != "" {
				return id, nil
			}
		}
	}
	return "", errors.New("no machine ID (/etc/machine-id is missing or empty; run systemd-machine-id-setup)")
}

// bootID changes on every boot; the license server uses it to spot one seat
// used by two machines (a cloned VM).
func bootID() string { return readFirstLine("/proc/sys/kernel/random/boot_id") }
