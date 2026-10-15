//go:build windows

package main

import "golang.org/x/sys/windows"

const unixLike = false

// isElevated reports whether hotfixd runs with administrator rights (always
// true for the service, which runs as LocalSystem).
func isElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

func selfUID() uint32 { return 0 }
