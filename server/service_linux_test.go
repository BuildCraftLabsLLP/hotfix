//go:build linux

package main

import (
	"strings"
	"testing"
)

func TestSystemdUnitTemplate(t *testing.T) {
	tpl := systemdUnitTemplate()
	if !strings.Contains(tpl, "ExecStart={{Path | cmdEscape}} run") || strings.Contains(tpl, "/usr/bin/hotfixd run") {
		t.Fatalf("ExecStart not templated:\n%s", tpl)
	}
	if strings.Contains(tpl, "\r") {
		t.Error("unit has CRLF line endings")
	}
	for _, must := range []string{"CAP_KILL", "CAP_SYS_PTRACE", "ExecReload=/bin/kill -HUP $MAINPID", "StateDirectory=hotfix-server"} {
		if !strings.Contains(tpl, must) {
			t.Errorf("unit lacks %q", must)
		}
	}
}
