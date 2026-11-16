//go:build !windows

package tsnode

import "os"

// SecureDir restricts dir to the owning user.
func SecureDir(dir string) error { return os.Chmod(dir, 0o700) }
