// Package host implements the rmtd side of a session: admission control,
// handshake, and dispatch of typed streams to handlers.
package host

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"time"

	"rmt-desk/internal/audit"
	"rmt-desk/internal/authz"
	"rmt-desk/internal/proto"
)

// StreamHandler serves one logical stream. It owns conn and must close it.
type StreamHandler func(ctx context.Context, id authz.Identity, conn net.Conn)

type Server struct {
	Policy   authz.Policy
	Resolver authz.Resolver
	Audit    *audit.Log
	HostName string
	Handlers map[string]StreamHandler // keyed by proto.Stream* kind
	Logf     func(string, ...any)
}

func (s *Server) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	} else {
		log.Printf(f, a...)
	}
}

// Serve accepts connections until ln is closed or ctx is done.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handleConn(ctx, c)
	}
}

func (s *Server) handleConn(ctx context.Context, c net.Conn) {
	defer c.Close()
	remote := c.RemoteAddr().String()

	// 1. Admission: tailnet identity must be allowlisted before we read a byte.
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	id, err := s.Policy.Check(actx, s.Resolver, remote)
	cancel()
	if err != nil {
		s.logf("deny %s: %v", remote, err)
		s.Audit.Write(audit.Event{Kind: "deny", Peer: id.String(), Remote: remote, Detail: err.Error()})
		_ = proto.WriteJSON(c, proto.MsgError, proto.Error{Message: "access denied"})
		return
	}

	// 2. Handshake (bounded in time).
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	var hello proto.Hello
	if err := proto.ReadJSON(c, proto.MsgHello, &hello); err != nil {
		s.logf("handshake %s: %v", id, err)
		return
	}
	if hello.Version != proto.Version {
		_ = proto.WriteJSON(c, proto.MsgError, proto.Error{
			Message: fmt.Sprintf("protocol version mismatch: host=%d viewer=%d", proto.Version, hello.Version)})
		return
	}
	// TODO(phase3): unattended password/TOTP + attended consent prompt.
	granted := []string{"view", "input", "clipboard", "files"}
	host := s.HostName
	if host == "" {
		host, _ = os.Hostname()
	}
	if err := proto.WriteJSON(c, proto.MsgHelloAck, proto.HelloAck{
		Version: proto.Version, Host: host, OS: runtime.GOOS, Granted: granted, Identity: id.String(),
	}); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})

	s.logf("session start: %s (%s)", id, hello.Client)
	s.Audit.Write(audit.Event{Kind: "connect", Peer: id.String(), Remote: remote, Detail: hello.Client})
	start := time.Now()
	defer func() {
		s.Audit.Write(audit.Event{Kind: "disconnect", Peer: id.String(), Remote: remote,
			Detail: time.Since(start).Round(time.Second).String()})
		s.logf("session end: %s", id)
	}()

	// 3. Multiplexed streams.
	sess, err := proto.ServerSession(c)
	if err != nil {
		return
	}
	defer sess.Close()
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	for {
		st, kind, err := proto.AcceptStream(sess)
		if err != nil {
			return
		}
		h, ok := s.Handlers[kind]
		if !ok {
			_ = proto.WriteJSON(st, proto.MsgError, proto.Error{Message: "unsupported stream: " + kind})
			st.Close()
			continue
		}
		go h(sctx, id, st)
	}
}
