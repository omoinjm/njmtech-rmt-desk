package host

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"rmt-desk/internal/access"
	"rmt-desk/internal/authz"
	"rmt-desk/internal/proto"
	"rmt-desk/internal/viewer"
)

// staticResolver returns the same identity for every peer.
type staticResolver struct {
	id  authz.Identity
	err error
}

func (r staticResolver) Resolve(context.Context, string) (authz.Identity, error) { return r.id, r.err }

func startServer(t *testing.T, r authz.Resolver) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Server{
		Policy:   authz.Policy{AllowedLogins: []string{"alice@example.com"}},
		Gate:     autoGate(),
		Resolver: r,
		HostName: "testhost",
		Logf:     t.Logf,
		Handlers: map[string]StreamHandler{
			// echo handler for testing stream dispatch
			proto.StreamControl: func(_ context.Context, _ authz.Identity, c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			},
		},
	}
	done := make(chan struct{})
	go func() { s.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String()
}

var tcpDial viewer.Dialer = (&net.Dialer{}).DialContext

func TestAllowedPeerHandshakeAndStream(t *testing.T) {
	addr := startServer(t, staticResolver{id: authz.Identity{Login: "alice@example.com", NodeName: "laptop"}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := viewer.Connect(ctx, tcpDial, addr, viewer.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Info.Host != "testhost" || s.Info.Version != proto.Version {
		t.Fatalf("bad ack: %+v", s.Info)
	}
	st, err := s.Open(proto.StreamControl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(st, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo failed: %q %v", buf, err)
	}
}

func TestDeniedPeer(t *testing.T) {
	addr := startServer(t, staticResolver{id: authz.Identity{Login: "mallory@example.com"}})
	_, err := viewer.Connect(context.Background(), tcpDial, addr, viewer.Auth{})
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("expected access denied, got %v", err)
	}
}

func TestWhoIsFailureDenied(t *testing.T) {
	addr := startServer(t, staticResolver{err: errors.New("no such peer")})
	if _, err := viewer.Connect(context.Background(), tcpDial, addr, viewer.Auth{}); err == nil {
		t.Fatal("expected denial when WhoIs fails")
	}
}

func TestUnsupportedStream(t *testing.T) {
	addr := startServer(t, staticResolver{id: authz.Identity{Login: "alice@example.com"}})
	s, err := viewer.Connect(context.Background(), tcpDial, addr, viewer.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, err := s.Open("bogus")
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.ReadJSON(st, proto.MsgOpen, &proto.StreamOpen{}); err == nil ||
		!strings.Contains(err.Error(), "unsupported stream") {
		t.Fatalf("expected unsupported stream error, got %v", err)
	}
}

func TestPermissionEnforced(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	g := autoGate()
	g.Config.Permissions = []string{"view"} // no input
	s := &Server{
		Policy: authz.Policy{AllowedLogins: []string{"alice@example.com"}}, Gate: g,
		Resolver: staticResolver{id: authz.Identity{Login: "alice@example.com"}}, Logf: t.Logf,
		Handlers: map[string]StreamHandler{
			proto.StreamInput: func(_ context.Context, _ authz.Identity, c net.Conn) { c.Close(); t.Error("input handler ran") },
		},
	}
	done := make(chan struct{})
	go func() { s.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	sess, err := viewer.Connect(ctx, tcpDial, ln.Addr().String(), viewer.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if len(sess.Info.Granted) != 1 || sess.Info.Granted[0] != "view" {
		t.Fatalf("granted %v", sess.Info.Granted)
	}
	st, _ := sess.Open(proto.StreamInput)
	if err := proto.ReadJSON(st, proto.MsgOpen, &proto.StreamOpen{}); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("want permission denied, got %v", err)
	}
}

func TestConsentCancelledWhenViewerHangsUp(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan struct{})
	g := &access.Gate{Consent: func(ctx context.Context, _ string, _ time.Duration) (bool, error) {
		<-ctx.Done()
		close(cancelled)
		return false, nil
	}}
	s := &Server{Policy: authz.Policy{AllowedLogins: []string{"alice@example.com"}}, Gate: g,
		Resolver: staticResolver{id: authz.Identity{Login: "alice@example.com"}}, Logf: t.Logf}
	done := make(chan struct{})
	go func() { s.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	proto.WriteJSON(c, proto.MsgHello, proto.Hello{Version: proto.Version})
	time.Sleep(100 * time.Millisecond)
	c.Close()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("consent prompt not cancelled after viewer hung up")
	}
}
