//go:build windows

package hostsetup

import (
	"errors"

	"golang.org/x/sys/windows"
)

// serviceStateNative queries the SCM with minimal rights. kardianos'
// Status asks for full SCM access, which a non-elevated user (the app)
// doesn't have, and would misreport "not installed".
func serviceStateNative() (string, bool) {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return "", false
	}
	defer windows.CloseServiceHandle(m)
	name, _ := windows.UTF16PtrFromString(ServiceName)
	h, err := windows.OpenService(m, name, windows.SERVICE_QUERY_STATUS)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return NotInstalled, true
	}
	if err != nil {
		return "", false
	}
	defer windows.CloseServiceHandle(h)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(h, &st); err != nil {
		return "", false
	}
	if st.CurrentState == windows.SERVICE_RUNNING {
		return Running, true
	}
	return Stopped, true
}
