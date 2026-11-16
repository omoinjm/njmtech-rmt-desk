//go:build windows

package tsnode

import "golang.org/x/sys/windows"

// SecureDir restricts dir to SYSTEM and Administrators (protected DACL, no
// inheritance from %ProgramData%, which is readable by all users). The
// directory holds the node's WireGuard private key and the password hash.
func SecureDir(dir string) error {
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
