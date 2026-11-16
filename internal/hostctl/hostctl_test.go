//go:build !windows

package hostctl

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"rmt-desk/internal/access"
	"rmt-desk/internal/authz"
	"rmt-desk/internal/host"
	"rmt-desk/internal/proto"
	"rmt-desk/internal/viewer"
)

type resolver struct{}

func (resolver) Resolve(context.Context, string) (authz.Identity, error) {
	return authz.Identity{Login: "alice@example.com", NodeName: "laptop"}, nil
}

type handler struct{ srv *host.Server }

func (h handler) Status(context.Context) Status {
	return Status{HostName: "rmt-test", Online: true, Sessions: h.srv.Sessions()}
}
func (h handler) DisconnectAll() int { return h.srv.DisconnectAll() }

// shortDir: unix socket paths are limited to ~104 bytes on macOS.
func shortDir(t *testing.T) string {
	d, err := os.MkdirTemp("", "hc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestStatusAndDisconnectEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := &host.Server{
		Policy:   authz.Policy{AllowedLogins: []string{"alice@example.com"}},
		Gate:     &access.Gate{Consent: func(context.Context, string, time.Duration) (bool, error) { return true, nil }},
		Resolver: resolver{},
		Logf:     t.Logf,
		Handlers: map[string]host.StreamHandler{proto.StreamControl: func(_ context.Context, _ authz.Identity, c net.Conn) {
			defer c.Close()
			io.Copy(c, c)
		}},
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	done := make(chan struct{})
	go func() { srv.Serve(ctx, ln); close(done) }()
	defer func() { cancel(); <-done }()

	dir := shortDir(t)
	cl, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	go Serve(ctx, cl, handler{srv})
	if fi, err := os.Stat(filepath.Join(dir, "hostctl.sock")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket perms: %v %v", fi.Mode(), err)
	}
	if _, err := Listen(dir); err == nil {
		t.Fatal("second Listen on a live socket should fail")
	}

	st, err := GetStatus(ctx, dir)
	if err != nil || st.HostName != "rmt-test" || len(st.Sessions) != 0 {
		t.Fatalf("status before: %+v %v", st, err)
	}

	sess, err := viewer.Connect(ctx, (&net.Dialer{}).DialContext, ln.Addr().String(), viewer.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	ctl, err := sess.Open(proto.StreamControl)
	if err != nil {
		t.Fatal(err)
	}
	st, _ = GetStatus(ctx, dir)
	if len(st.Sessions) != 1 || st.Sessions[0].Peer != "laptop (alice@example.com)" {
		t.Fatalf("status during session: %+v", st.Sessions)
	}

	n, err := Disconnect(ctx, dir)
	if err != nil || n != 1 {
		t.Fatalf("disconnect: %d %v", n, err)
	}
	ctl.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := ctl.Read(make([]byte, 1)); err == nil {
		t.Fatal("stream still open after disconnect")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, _ = GetStatus(ctx, dir)
		if len(st.Sessions) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session still listed after disconnect")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestNotRunning(t *testing.T) {
	if _, err := GetStatus(context.Background(), shortDir(t)); err == nil {
		t.Fatal("expected ErrNotRunning")
	}
}
