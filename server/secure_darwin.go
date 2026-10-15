//go:build darwin

package main

// The support directory is created by root with 0755; nothing extra needed.
func secureDataDir(string) error { return nil }
