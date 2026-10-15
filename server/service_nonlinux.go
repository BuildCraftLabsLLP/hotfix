//go:build !linux

package main

func systemdUnitTemplate() string { return "" }

func packagedServiceCommand(string) (bool, error) { return false, nil }

func packagedServiceState() (string, bool) { return "", false }
