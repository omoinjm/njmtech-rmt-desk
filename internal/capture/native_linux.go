//go:build linux

package capture

func newNative() (Capturer, error) { return NewX11() }
