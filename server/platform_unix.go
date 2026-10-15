//go:build linux || darwin

package main

import "os"

// unixLike: processes have numeric owners, and without root only your own can
// be signalled.
const unixLike = true

// isElevated reports whether hotfixd runs as root.
func isElevated() bool { return os.Geteuid() == 0 }

func selfUID() uint32 { return uint32(os.Getuid()) }
