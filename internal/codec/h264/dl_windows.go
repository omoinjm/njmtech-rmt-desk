//go:build windows && (amd64 || arm64)

package h264

import "syscall"

func dlopen(path string) (uintptr, error) {
	// Full path: no DLL search-order lookup.
	d, err := syscall.LoadDLL(path)
	if err != nil {
		return 0, err
	}
	return uintptr(d.Handle), nil
}

func dlsym(h uintptr, name string) (uintptr, error) {
	return syscall.GetProcAddress(syscall.Handle(h), name)
}

func callN(fn uintptr, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(fn, args...)
	return r
}
