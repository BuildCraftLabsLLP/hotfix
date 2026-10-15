//go:build windows

package main

import (
	"errors"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// machineID is the MachineGuid Windows creates at install (64-bit view, so a
// 32-bit process reads the same value).
func machineID() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return "", err
	}
	if v = strings.TrimSpace(v); v == "" {
		return "", errors.New("MachineGuid is empty")
	}
	return v, nil
}

func bootID() string { return "" }
