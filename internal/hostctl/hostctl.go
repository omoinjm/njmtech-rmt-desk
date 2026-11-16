// Package hostctl is the local control channel between a running rmtd and
// the rmt-desk app on the same machine. It carries no secrets: status for
// display, and "disconnect" so the person at the host can end a session.
//
// Transport: a named pipe on Windows (SYSTEM/Administrators full control,
// interactive users read/write; created first-instance-only so another
// process can't squat the name before rmtd), a 0600 unix socket in the
// host's state dir elsewhere.
package hostctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"rmt-desk/internal/host"
)

// Status is what rmtd reports about itself.
type Status struct {
	Version     string             `json:"version"`
	HostName    string             `json:"hostName"`    // configured node name
	TailnetName string             `json:"tailnetName"` // MagicDNS name
	IP          string             `json:"ip"`
	Online      bool               `json:"online"`
	Mode        string             `json:"mode"` // effective access mode
	PasswordSet bool               `json:"passwordSet"`
	TOTPSet     bool               `json:"totpSet"`
	Allowed     []string           `json:"allowed"`
	Sessions    []host.SessionInfo `json:"sessions"`
}

// Handler is implemented by rmtd.
type Handler interface {
	Status(ctx context.Context) Status
	DisconnectAll() int
}

type request struct {
	Op string `json:"op"` // "status" | "disconnect"
}

type response struct {
	Status       *Status `json:"status,omitempty"`
	Disconnected int     `json:"disconnected,omitempty"`
	Error        string  `json:"error,omitempty"`
}

// Serve answers requests on ln until ctx is done.
func Serve(ctx context.Context, ln net.Listener, h Handler) error {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go handle(ctx, c, h)
	}
}

func handle(ctx context.Context, c net.Conn, h Handler) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(&limitReader{c, 4096}).ReadBytes('\n')
	if err != nil {
		return
	}
	var req request
	var resp response
	if json.Unmarshal(line, &req) != nil {
		resp.Error = "bad request"
	} else {
		switch req.Op {
		case "status":
			st := h.Status(ctx)
			resp.Status = &st
		case "disconnect":
			resp.Disconnected = h.DisconnectAll()
		default:
			resp.Error = "unknown op"
		}
	}
	_ = json.NewEncoder(c).Encode(resp)
}

type limitReader struct {
	c net.Conn
	n int
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errors.New("request too large")
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	n, err := l.c.Read(p)
	l.n -= n
	return n, err
}

// ErrNotRunning: no rmtd is listening.
var ErrNotRunning = errors.New("rmtd is not running")

func call(ctx context.Context, dir string, req request) (response, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := dial(ctx, dir)
	if err != nil {
		return response{}, fmt.Errorf("%w (%v)", ErrNotRunning, err)
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return response{}, err
	}
	var resp response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return response{}, err
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// GetStatus asks the local rmtd for its status. dir is the host state dir
// (used for the unix socket path; ignored on Windows).
func GetStatus(ctx context.Context, dir string) (Status, error) {
	resp, err := call(ctx, dir, request{Op: "status"})
	if err != nil {
		return Status{}, err
	}
	if resp.Status == nil {
		return Status{}, errors.New("empty status")
	}
	return *resp.Status, nil
}

// Disconnect ends all sessions on the local host.
func Disconnect(ctx context.Context, dir string) (int, error) {
	resp, err := call(ctx, dir, request{Op: "disconnect"})
	return resp.Disconnected, err
}
