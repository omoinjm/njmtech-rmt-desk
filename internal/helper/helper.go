// Package helper splits rmtd into a privileged-but-blind service and an
// unprivileged session helper that can see the user's desktop.
//
// On Windows a service runs in session 0, which has no access to the
// interactive desktop. The service therefore launches `rmtd helper` inside
// the active console session (with the logged-in user's token) and talks to
// it over the child's stdin/stdout, multiplexed with yamux. The tailnet,
// allowlist, password checks and audit log stay in the service; the helper
// only captures the screen, injects input and shows consent dialogs.
//
// The same mechanism is used in tests with an ordinary child process.
package helper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"rmt-desk/internal/authz"
	"rmt-desk/internal/proto"

	"github.com/hashicorp/yamux"
)

func muxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = false // local pipe; process exit closes it
	c.LogOutput = io.Discard
	return c
}

// Handler serves one stream inside the helper.
type Handler func(ctx context.Context, c net.Conn)

// Serve runs in the helper process until rwc closes or ctx is done.
func Serve(ctx context.Context, rwc io.ReadWriteCloser, handlers map[string]Handler) error {
	sess, err := yamux.Server(rwc, muxConfig())
	if err != nil {
		return err
	}
	defer sess.Close()
	context.AfterFunc(ctx, func() { sess.Close() })
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		st, kind, err := proto.AcceptStream(sess)
		if err != nil {
			if sess.IsClosed() {
				return nil
			}
			return err
		}
		h, ok := handlers[kind]
		if !ok {
			_ = proto.WriteJSON(st, proto.MsgError, proto.Error{Message: "helper: unsupported stream " + kind})
			st.Close()
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); h(ctx, st) }()
	}
}

// ConsentHandler adapts an Ask function to the consent stream.
func ConsentHandler(ask func(ctx context.Context, who string, timeout time.Duration) (bool, error)) Handler {
	return func(ctx context.Context, c net.Conn) {
		defer c.Close()
		var req proto.ConsentReq
		if err := proto.ReadJSON(c, proto.MsgConsentReq, &req); err != nil {
			return
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		// If the service gives up (viewer hung up), the stream closes: stop asking.
		go func() { var b [1]byte; c.Read(b[:]); cancel() }()
		ok, err := ask(ctx, req.Who, time.Duration(req.TimeoutMS)*time.Millisecond)
		resp := proto.ConsentResp{Allow: ok && err == nil}
		if err != nil {
			resp.Error = err.Error()
		}
		_ = proto.WriteJSON(c, proto.MsgConsentResp, resp)
	}
}

// Proc is a running helper process.
type Proc interface {
	io.ReadWriteCloser
	Wait() error
	Kill() error
}

// Manager owns the helper process on the service side, (re)starting it on
// demand: after a crash, or when the active console session changes.
type Manager struct {
	// Spawn starts a helper in the current interactive session.
	Spawn func() (Proc, error)
	// Session reports the current interactive session id; a change makes
	// the manager replace the helper. Optional.
	Session func() uint32
	Logf    func(string, ...any)

	mu      sync.Mutex
	proc    Proc
	mux     *yamux.Session
	exited  chan struct{} // closed once proc has been reaped
	session uint32
}

func (m *Manager) logf(f string, a ...any) {
	if m.Logf != nil {
		m.Logf(f, a...)
	} else {
		log.Printf(f, a...)
	}
}

func (m *Manager) get() (*yamux.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var sid uint32
	if m.Session != nil {
		sid = m.Session()
	}
	if m.mux != nil && !m.mux.IsClosed() && sid == m.session {
		return m.mux, nil
	}
	if m.proc != nil {
		m.logf("helper: replacing (session %d -> %d, closed=%v)", m.session, sid, m.mux.IsClosed())
		m.stopLocked()
	}
	p, err := m.Spawn()
	if err != nil {
		return nil, fmt.Errorf("start session helper: %w", err)
	}
	mux, err := yamux.Client(p, muxConfig())
	if err != nil {
		p.Kill()
		return nil, err
	}
	exited := make(chan struct{})
	m.proc, m.mux, m.session, m.exited = p, mux, sid, exited
	go func() {
		defer close(exited)
		err := p.Wait()
		mux.Close()
		m.logf("helper exited: %v", err)
	}()
	m.logf("helper started (session %d)", sid)
	return mux, nil
}

func (m *Manager) stopLocked() {
	if m.mux != nil {
		m.mux.Close()
	}
	if m.proc != nil {
		m.proc.Close() // EOF on the helper's stdin => it exits cleanly
		select {
		case <-m.exited:
		case <-time.After(2 * time.Second):
			m.proc.Kill()
			<-m.exited
		}
	}
	m.proc, m.mux, m.exited = nil, nil, nil
}

// Close stops the helper.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

func (m *Manager) open(kind string) (net.Conn, error) {
	mux, err := m.get()
	if err != nil {
		return nil, err
	}
	st, err := proto.OpenStream(mux, kind)
	if err != nil {
		// Stale session (helper died between checks): retry once.
		m.mu.Lock()
		m.stopLocked()
		m.mu.Unlock()
		if mux, err = m.get(); err != nil {
			return nil, err
		}
		st, err = proto.OpenStream(mux, kind)
	}
	return st, err
}

// Proxy returns a host stream handler that forwards the viewer's stream to
// the same kind of stream in the helper.
func (m *Manager) Proxy(kind string) func(ctx context.Context, id authz.Identity, c net.Conn) {
	return func(ctx context.Context, id authz.Identity, c net.Conn) {
		defer c.Close()
		st, err := m.open(kind)
		if err != nil {
			m.logf("helper %s: %v", kind, err)
			_ = proto.WriteJSON(c, proto.MsgError, proto.Error{Message: err.Error()})
			return
		}
		defer st.Close()
		pipe(ctx, c, st)
	}
}

// Consent asks the helper to show the consent dialog.
func (m *Manager) Consent(ctx context.Context, who string, timeout time.Duration) (bool, error) {
	st, err := m.open(proto.StreamConsent)
	if err != nil {
		return false, err
	}
	defer st.Close()
	context.AfterFunc(ctx, func() { st.Close() })
	if err := proto.WriteJSON(st, proto.MsgConsentReq, proto.ConsentReq{Who: who, TimeoutMS: timeout.Milliseconds()}); err != nil {
		return false, err
	}
	var resp proto.ConsentResp
	if err := proto.ReadJSON(st, proto.MsgConsentResp, &resp); err != nil {
		if ctx.Err() != nil {
			return false, nil
		}
		return false, err
	}
	if resp.Error != "" {
		return false, errors.New(resp.Error)
	}
	return resp.Allow, nil
}

// pipe copies both ways until either side ends or ctx is done.
func pipe(ctx context.Context, a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) { io.Copy(dst, src); done <- struct{}{} }
	go cp(a, b)
	go cp(b, a)
	select {
	case <-done:
	case <-ctx.Done():
	}
	a.Close()
	b.Close()
	<-done
}

// StdioConn joins a reader and writer (e.g. os.Stdin/os.Stdout) into one
// ReadWriteCloser for Serve.
type StdioConn struct {
	R io.ReadCloser
	W io.WriteCloser
}

func (s StdioConn) Read(p []byte) (int, error)  { return s.R.Read(p) }
func (s StdioConn) Write(p []byte) (int, error) { return s.W.Write(p) }
func (s StdioConn) Close() error {
	e1 := s.R.Close()
	e2 := s.W.Close()
	return errors.Join(e1, e2)
}
