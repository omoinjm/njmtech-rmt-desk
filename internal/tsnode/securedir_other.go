//go:build !windows

package tsnode

import "os"

// SecureDir restricts dir to the owning user.
func SecureDir(dir string) error { return os.Chmod(dir, 0o700) }

// SecureSharedDir: owner-writable, readable by others.
func SecureSharedDir(dir string) error { return os.Chmod(dir, 0o755) }

// SecureUserDir restricts dir to the owning user.
func SecureUserDir(dir string) error { return os.Chmod(dir, 0o700) }
