//go:build !windows

package main

// On macOS/Linux the service is a per-user agent inside the graphical
// session, so everything runs in-process.
func newBackend(dir string, serviceMode bool) (*backend, error) {
	return inProcessBackend(codecDir(dir)), nil
}
