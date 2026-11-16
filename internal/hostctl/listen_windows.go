//go:build windows

package hostctl

import (
	"context"
	"net"

	winio "github.com/tailscale/go-winio"
)

const pipeName = `\\.\pipe\rmt-desk-hostctl`

// Pipe DACL: SYSTEM and Administrators full control; interactive users
// (the person at the console) read/write so the app can show status and
// end sessions without elevation.
const pipeSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;IU)"

// Listen creates the control pipe. winio creates the first instance with
// FILE_FLAG_FIRST_PIPE_INSTANCE, so this fails if the name is squatted.
func Listen(string) (net.Listener, error) {
	return winio.ListenPipe(pipeName, &winio.PipeConfig{SecurityDescriptor: pipeSDDL})
}

func dial(ctx context.Context, _ string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, pipeName)
}
