//go:build linux

package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

//go:embed packaging/hotfixd.service
var systemdUnit string

// systemdUnitTemplate is the packaged unit as a kardianos/service template,
// with the binary path filled in for wherever this hotfixd lives (tarball
// installs put it in /usr/local/bin).
func systemdUnitTemplate() string {
	return strings.Replace(systemdUnit, "ExecStart=/usr/bin/hotfixd run", "ExecStart={{Path | cmdEscape}} run", 1)
}

// packagedUnit is where the .deb/.rpm installs the unit.
var packagedUnits = []string{"/usr/lib/systemd/system/hotfixd.service", "/lib/systemd/system/hotfixd.service"}

func packagedUnitInstalled() bool {
	for _, p := range packagedUnits {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

// packagedServiceCommand drives the unit the package installed, so a package
// install never ends up with a second unit in /etc/systemd/system.
func packagedServiceCommand(cmd string) (bool, error) {
	if !packagedUnitInstalled() {
		return false, nil
	}
	switch cmd {
	case "install":
		if err := prepareDataDirs(); err != nil {
			return true, err
		}
		if err := systemctl("enable", "--now", "hotfixd"); err != nil {
			return true, err
		}
		printInstalled()
		return true, nil
	case "uninstall":
		err := systemctl("disable", "--now", "hotfixd")
		if err == nil {
			fmt.Println("Hotfix Server is stopped and disabled. Remove the package to uninstall it completely.")
		}
		return true, err
	default:
		return true, systemctl(cmd, "hotfixd")
	}
}

func packagedServiceState() (string, bool) {
	if !packagedUnitInstalled() {
		return "", false
	}
	out, _ := exec.Command("systemctl", "is-active", "hotfixd").Output()
	state := strings.TrimSpace(string(out))
	switch state {
	case "active":
		return "running", true
	case "":
		return "unknown", true
	default:
		return state + " (run `sudo hotfixd start`)", true
	}
}

func secureDataDir(string) error { return nil }
