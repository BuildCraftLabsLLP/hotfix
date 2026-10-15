//go:build windows

package main

import "golang.org/x/sys/windows"

// restrictToAdmins limits a file to SYSTEM and Administrators (the data
// directory itself lets Users read, for status.json and the config). Only
// when elevated: a non-admin process would lock itself out of its own file.
func restrictToAdmins(path string) error {
	if !isElevated() {
		return nil
	}
	sd, err := windows.SecurityDescriptorFromString("D:PAI(A;;FA;;;SY)(A;;FA;;;BA)")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
