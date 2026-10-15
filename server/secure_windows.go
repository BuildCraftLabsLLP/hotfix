//go:build windows

package main

import "golang.org/x/sys/windows"

// dataDirSDDL: SYSTEM and Administrators full control, Users read-only,
// inherited by everything inside, nothing inherited from %ProgramData% (whose
// default lets any user create files, e.g. a config whitelisting themselves).
const dataDirSDDL = "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)"

func secureDataDir(dir string) error {
	sd, err := windows.SecurityDescriptorFromString(dataDirSDDL)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil)
}
