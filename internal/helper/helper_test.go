package helper_test

import (
	"context"
	"image"
	"net"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"rmt-desk/internal/access"
	"rmt-desk/internal/authz"
	"rmt-desk/internal/capture"
	"rmt-desk/internal/helper"
	"rmt-desk/internal/host"
	"rmt-desk/internal/input"
	"rmt-desk/internal/proto"
	"rmt-desk/internal/viewer"
)

type screen struct{}

func (screen) Displays() []capture.Display {
	return []capture.Display{{Index: 0, Bounds: image.Rect(0, 0, 64, 64), Width: 64, Height: 64}}
}
func (screen) Capture(int) (*image.RGBA, error) { return image.NewRGBA(image.Rect(0, 0, 64, 64)), nil }

// TestMain doubles as the helper binary: the manager re-executes the test
// binary with RMT_HELPER_TEST set, exactly like `rmtd helper`.
func TestMain(m *testing.M) {
	if mode := os.Getenv("RMT_HELPER_TEST"); mode != "" {
		desk := &host.Desktop{Capturer: screen{},
			NewInjector: func() (input.Injector, error) { return nil, os.ErrPermission }}
		err := helper.Serve(context.Background(), helper.StdioConn{R: os.Stdin, W: os.Stdout}, map[string]helper.Handler{
			proto.StreamVideo: func(ctx context.Context, c net.Conn) { desk.Video(ctx, authz.Identity{}, c) },
			proto.StreamConsent: helper.ConsentHandler(func(context.Context, string, time.Duration) (bool, error) {
				return mode == "allow", nil
			}),
		})
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func newManager(t *testing.T, mode string, spawns *atomic.Int32) *helper.Manager {
	m := &helper.Manager{
		Logf: t.Logf,
		Spawn: func() (helper.Proc, error) {
			spawns.Add(1)
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), "RMT_HELPER_TEST="+mode)
			cmd.Stderr = os.Stderr
			return helper.SpawnExec(cmd)
		},
	}
	t.Cleanup(m.Close)
	return m
}

func TestConsentThroughHelper(t *testing.T) {
	var n atomic.Int32
	for mode, want := range map[string]bool{"allow": true, "deny": false} {
		m := newManager(t, mode, &n)
		ok, err := m.Consent(context.Background(), "alice", time.Second)
		if err != nil || ok != want {
			t.Fatalf("%s: ok=%v err=%v", mode, ok, err)
		}
	}
}

func TestVideoProxiedThroughHelperAndRespawn(t *testing.T) {
	var spawns atomic.Int32
	var session atomic.Uint32
	m := newManager(t, "allow", &spawns)
	m.Session = func() uint32 { return session.Load() }

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	srv := &host.Server{
		Policy:   authz.Policy{AllowedLogins: []string{"alice@example.com"}},
		Resolver: resolver{},
		Gate:     &access.Gate{Consent: m.Consent}, // consent shown by the helper
		Logf:     t.Logf,
		Handlers: map[string]host.StreamHandler{proto.StreamVideo: m.Proxy(proto.StreamVideo)},
	}
	done := make(chan struct{})
	go func() { srv.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	getFrame := func() {
		t.Helper()
		sess, err := viewer.Connect(ctx, (&net.Dialer{}).DialContext, ln.Addr().String(), viewer.Auth{})
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		st, _ := sess.Open(proto.StreamVideo)
		st.SetDeadline(time.Now().Add(5 * time.Second))
		proto.WriteJSON(st, proto.MsgVideoReq, proto.VideoReq{FPS: 30})
		var info proto.VideoInfo
		if err := proto.ReadJSON(st, proto.MsgVideoInfo, &info); err != nil {
			t.Fatal(err)
		}
		if typ, _, err := proto.ReadFrame(st); err != nil || typ != proto.MsgVideoFrame {
			t.Fatalf("frame: %d %v", typ, err)
		}
	}

	getFrame()
	getFrame()
	if n := spawns.Load(); n != 1 {
		t.Fatalf("helper should be reused, spawned %d", n)
	}
	// User switch / new console session => fresh helper in the new session.
	session.Store(2)
	getFrame()
	if n := spawns.Load(); n != 2 {
		t.Fatalf("session change: spawned %d, want 2", n)
	}
}

func TestHelperCrashRecovery(t *testing.T) {
	var spawns atomic.Int32
	var procs []helper.Proc
	m := &helper.Manager{Logf: t.Logf}
	m.Spawn = func() (helper.Proc, error) {
		spawns.Add(1)
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), "RMT_HELPER_TEST=allow")
		p, err := helper.SpawnExec(cmd)
		procs = append(procs, p)
		return p, err
	}
	t.Cleanup(m.Close)
	if ok, err := m.Consent(context.Background(), "x", time.Second); !ok || err != nil {
		t.Fatal(ok, err)
	}
	procs[0].Kill()
	time.Sleep(200 * time.Millisecond)
	if ok, err := m.Consent(context.Background(), "x", time.Second); !ok || err != nil {
		t.Fatalf("after crash: %v %v", ok, err)
	}
	if spawns.Load() != 2 {
		t.Fatalf("spawns=%d want 2", spawns.Load())
	}
}

type resolver struct{}

func (resolver) Resolve(context.Context, string) (authz.Identity, error) {
	return authz.Identity{Login: "alice@example.com"}, nil
}
