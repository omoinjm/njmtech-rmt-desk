//go:build !windows

package hostctl

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
)

func sockPath(dir string) string { return filepath.Join(dir, "hostctl.sock") }

// Listen creates the control socket in dir (0600; dir itself is 0700).
func Listen(dir string) (net.Listener, error) {
	p := sockPath(dir)
	// A previous rmtd may have left the socket behind; only remove it if
	// nobody answers.
	if c, err := net.Dial("unix", p); err == nil {
		c.Close()
		return nil, errors.New("another rmtd is already serving " + p)
	}
	_ = os.Remove(p)
	old := umask(0o177)
	ln, err := net.Listen("unix", p)
	umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(p, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func dial(ctx context.Context, dir string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", sockPath(dir))
}
