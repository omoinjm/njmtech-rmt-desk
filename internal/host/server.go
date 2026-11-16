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
	"slices"
	"sync"
	"time"

	"rmt-desk/internal/access"
	"rmt-desk/internal/audit"
	"rmt-desk/internal/authz"
	"rmt-desk/internal/proto"
)

// StreamHandler serves one logical stream. It owns conn and must close it.
type StreamHandler func(ctx context.Context, id authz.Identity, conn net.Conn)

type Server struct {
	Policy   authz.Policy
	Gate     *access.Gate // password/TOTP/consent; required
	Resolver authz.Resolver
	Audit    *audit.Log
	HostName string
	Handlers map[string]StreamHandler // keyed by proto.Stream* kind
	Logf     func(string, ...any)

	mu       sync.Mutex
	sessions map[*liveSession]struct{}
}

// SessionInfo describes an admitted session, for local status display.
type SessionInfo struct {
	Peer    string    `json:"peer"` // identity, e.g. "you@example.com (laptop)"
	Since   time.Time `json:"since"`
	Granted []string  `json:"granted"`
}

type liveSession struct {
	info SessionInfo
	conn net.Conn
}

// Sessions lists the sessions currently admitted.
func (s *Server) Sessions() []SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionInfo, 0, len(s.sessions))
	for ls := range s.sessions {
		out = append(out, ls.info)
	}
	slices.SortFunc(out, func(a, b SessionInfo) int { return a.Since.Compare(b.Since) })
	return out
}

// DisconnectAll ends every admitted session (the host user's kill switch)
// and returns how many were ended.
func (s *Server) DisconnectAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ls := range s.sessions {
		ls.conn.Close()
	}
	return len(s.sessions)
}

func (s *Server) track(ls *liveSession) (untrack func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = map[*liveSession]struct{}{}
	}
	s.sessions[ls] = struct{}{}
	return func() {
		s.mu.Lock()
		delete(s.sessions, ls)
		s.mu.Unlock()
	}
}

func (s *Server) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	} else {
		log.Printf(f, a...)
	}
}

// Serve accepts connections until ln is closed or ctx is done. On return,
// all sessions have been torn down.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()

	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			unhook := context.AfterFunc(ctx, func() { c.Close() })
			defer unhook()
			s.handleConn(ctx, c)
		}()
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
		_ = proto.WriteJSON(c, proto.MsgError, proto.Error{Code: proto.CodeDenied, Message: "access denied"})
		return
	}

	// 2. Handshake. Bounded in time, but long enough for a consent dialog.
	_ = c.SetDeadline(time.Now().Add(90 * time.Second))
	var hello proto.Hello
	if err := proto.ReadJSON(c, proto.MsgHello, &hello); err != nil {
		s.logf("handshake %s: %v", id, err)
		return
	}
	if hello.Version != proto.Version {
		_ = proto.WriteJSON(c, proto.MsgError, proto.Error{Code: proto.CodeVersion,
			Message: fmt.Sprintf("protocol version mismatch: host=%d viewer=%d", proto.Version, hello.Version)})
		return
	}

	// 3. Session admission: password/TOTP or consent prompt.
	if s.Gate == nil {
		_ = proto.WriteJSON(c, proto.MsgError, proto.Error{Code: proto.CodeDenied, Message: "host has no access gate"})
		return
	}
	actx2, cancel2 := context.WithCancel(ctx)
	// If the viewer hangs up while a dialog is showing, stop waiting.
	stopWatch := watchClose(c, cancel2)
	granted, err := s.Gate.Admit(actx2, id, hello)
	stopWatch()
	cancel2()
	if err != nil {
		e := access.AsError(err)
		s.logf("refused %s: %s", id, e.Message)
		s.Audit.Write(audit.Event{Kind: "refuse", Peer: id.String(), Remote: remote, Detail: e.Code + ": " + e.Message})
		_ = proto.WriteJSON(c, proto.MsgError, e)
		return
	}
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
	s.Audit.Write(audit.Event{Kind: "connect", Peer: id.String(), Remote: remote,
		Detail: fmt.Sprintf("%s granted=%v", hello.Client, granted)})
	start := time.Now()
	untrack := s.track(&liveSession{info: SessionInfo{Peer: id.String(), Since: start, Granted: granted}, conn: c})
	defer untrack()
	defer func() {
		s.Audit.Write(audit.Event{Kind: "disconnect", Peer: id.String(), Remote: remote,
			Detail: time.Since(start).Round(time.Second).String()})
		s.logf("session end: %s", id)
	}()

	// 4. Multiplexed streams, each gated by the session's permissions.
	sess, err := proto.ServerSession(c)
	if err != nil {
		return
	}
	// Teardown order (LIFO): cancel handlers' ctx, close all streams, then
	// wait for handlers to return.
	var hwg sync.WaitGroup
	defer hwg.Wait()
	defer sess.Close()
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	for {
		st, kind, err := proto.AcceptStream(sess)
		if err != nil {
			return
		}
		h, ok := s.Handlers[kind]
		if ok && !slices.Contains(granted, streamPermission[kind]) {
			_ = proto.WriteJSON(st, proto.MsgError, proto.Error{Code: proto.CodeDenied, Message: "permission denied: " + kind})
			st.Close()
			continue
		}
		if !ok {
			_ = proto.WriteJSON(st, proto.MsgError, proto.Error{Message: "unsupported stream: " + kind})
			st.Close()
			continue
		}
		if kind != proto.StreamControl {
			s.Audit.Write(audit.Event{Kind: "stream", Peer: id.String(), Remote: remote, Detail: kind})
		}
		hwg.Add(1)
		go func() { defer hwg.Done(); h(sctx, id, st) }()
	}
}

// streamPermission maps each stream kind to the permission it needs.
var streamPermission = map[string]string{
	proto.StreamControl:   "view",
	proto.StreamVideo:     "view",
	proto.StreamInput:     "input",
	proto.StreamClipboard: "clipboard",
	proto.StreamFiles:     "files",
}

// watchClose cancels when the peer closes the connection while we are not
// otherwise reading from it (e.g. during a consent dialog). The returned
// func stops the watcher and restores normal reads.
func watchClose(c net.Conn, cancel func()) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var b [1]byte
		for {
			_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			_, err := c.Read(b[:])
			select {
			case <-done:
				return
			default:
			}
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				cancel()
				return
			}
			// The viewer must not send anything before HelloAck.
			cancel()
			return
		}
	}()
	return func() {
		close(done)
		<-finished
		_ = c.SetReadDeadline(time.Time{})
	}
}
