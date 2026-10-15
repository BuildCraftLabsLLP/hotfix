//go:build darwin

package main

import (
	"errors"
	"os/exec"
	"regexp"
)

var platformUUIDRe = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([0-9A-Fa-f-]+)"`)

// machineID is the hardware UUID (IOPlatformUUID).
func machineID() (string, error) {
	out, err := exec.Command("/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		return "", err
	}
	if m := platformUUIDRe.FindSubmatch(out); m != nil {
		return string(m[1]), nil
	}
	return "", errors.New("IOPlatformUUID not found")
}

func bootID() string { return "" }
