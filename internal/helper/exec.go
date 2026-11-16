package helper

import (
	"errors"
	"io"
	"os/exec"
)

// execProc runs a helper as an ordinary child process (tests, and any
// platform where the service already lives in the user's session).
type execProc struct {
	cmd *exec.Cmd
	StdioConn
}

// SpawnExec starts cmd with its stdin/stdout wired to the returned Proc.
// cmd.Stderr is left as configured by the caller (never stdout!).
func SpawnExec(cmd *exec.Cmd) (Proc, error) {
	w, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	r, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProc{cmd: cmd, StdioConn: StdioConn{R: r, W: w}}, nil
}

func (p *execProc) Wait() error { return p.cmd.Wait() }

func (p *execProc) Kill() error {
	if p.cmd.Process == nil {
		return errors.New("not started")
	}
	err := p.cmd.Process.Kill()
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
