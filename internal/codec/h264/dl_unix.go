//go:build (linux || darwin) && (amd64 || arm64)

package h264

import "github.com/ebitengine/purego"

func dlopen(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
}

func dlsym(h uintptr, name string) (uintptr, error) { return purego.Dlsym(h, name) }

func callN(fn uintptr, args ...uintptr) uintptr {
	r, _, _ := purego.SyscallN(fn, args...)
	return r
}
