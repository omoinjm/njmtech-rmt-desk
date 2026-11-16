//go:build !windows

package hostctl

import "syscall"

func umask(m int) int { return syscall.Umask(m) }
