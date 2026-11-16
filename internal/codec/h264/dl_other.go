//go:build !((linux || darwin || windows) && (amd64 || arm64))

package h264

import "errors"

var errUnsupported = errors.New("h264: not supported on this platform")

func dlopen(string) (uintptr, error)         { return 0, errUnsupported }
func dlsym(uintptr, string) (uintptr, error) { return 0, errUnsupported }
func callN(uintptr, ...uintptr) uintptr      { return 0 }
