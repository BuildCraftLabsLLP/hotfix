//go:build linux

package main

func configDir() string { return "/etc/hotfix-server" }
func stateDir() string  { return "/var/lib/hotfix-server" }
func logDir() string    { return "/var/log/hotfix-server" }
