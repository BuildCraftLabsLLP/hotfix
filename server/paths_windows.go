//go:build windows

package main

import (
	"os"
	"path/filepath"
)

// programDataDir is %ProgramData%\Hotfix Server. Its ACL is restricted on
// install (see secureDataDir) because ordinary users may create files under
// %ProgramData% by default.
func programDataDir() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "Hotfix Server")
}

func configDir() string { return programDataDir() }
func stateDir() string  { return programDataDir() }
func logDir() string    { return filepath.Join(programDataDir(), "logs") }
