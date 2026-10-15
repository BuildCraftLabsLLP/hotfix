//go:build darwin

package main

const darwinSupportDir = "/Library/Application Support/Hotfix Server"

func configDir() string { return darwinSupportDir }
func stateDir() string  { return darwinSupportDir }
func logDir() string    { return "/Library/Logs/Hotfix Server" }
