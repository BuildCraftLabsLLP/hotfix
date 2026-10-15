//go:build linux || darwin

package main

// restrictToAdmins: license.json is written 0600 by root already.
func restrictToAdmins(string) error { return nil }
