//go:build !windows

package hostsetup

func serviceStateNative() (string, bool) { return "", false }
