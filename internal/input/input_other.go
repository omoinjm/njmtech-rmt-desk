//go:build !linux && !windows && !(darwin && cgo)

package input

import "errors"

// New is unavailable on this platform/build (macOS requires CGO_ENABLED=1).
func New() (Injector, error) {
	return nil, errors.New("input: injection not supported in this build")
}
