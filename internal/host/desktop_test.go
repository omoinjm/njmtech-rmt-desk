package host

import (
	"context"
	"errors"
	"image"
	"image/color"
	"net"
	"sync"
	"testing"
	"time"

	"rmt-desk/internal/authz"
	"rmt-desk/internal/capture"
	"rmt-desk/internal/codec/tiles"
	"rmt-desk/internal/input"
	"rmt-desk/internal/proto"
	"rmt-desk/internal/viewer"
)

type fakeScreen struct {
	mu  sync.Mutex
	img *image.RGBA
}

func (f *fakeScreen) Displays() []capture.Display {
	b := f.img.Bounds()
	return []capture.Display{{Index: 0, Bounds: b, Width: b.Dx(), Height: b.Dy()}}
}

func (f *fakeScreen) Capture(int) (*image.RGBA, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *f.img
	cp.Pix = append([]byte(nil), f.img.Pix...)
	return &cp, nil
}

type recInjector struct {
	mu     sync.Mutex
	events []string
	closed bool
}

func (r *recInjector) add(s string)                       { r.mu.Lock(); r.events = append(r.events, s); r.mu.Unlock() }
func (r *recInjector) MoveTo(int, float64, float64) error { r.add("move"); return nil }
func (r *recInjector) Button(b input.Button, d bool) error {
	r.add(map[bool]string{true: "down", false: "up"}[d])
	return nil
}
func (r *recInjector) Scroll(int, int) error { r.add("scroll"); return nil }
func (r *recInjector) Key(k input.Key, d bool) error {
	r.add(map[bool]string{true: "kdown", false: "kup"}[d])
	return nil
}
func (r *recInjector) Close() error { r.mu.Lock(); r.closed = true; r.mu.Unlock(); return nil }
func (r *recInjector) snapshot() ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...), r.closed
}

func startDesktop(t *testing.T, scr *fakeScreen, inj *recInjector) *viewer.Session {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := &Desktop{Capturer: scr, NewInjector: func() (input.Injector, error) { return inj, nil }, Logf: t.Logf}
	s := &Server{
		Policy:   authz.Policy{AllowedLogins: []string{"alice@example.com"}},
		Gate:     autoGate(),
		Resolver: staticResolver{id: authz.Identity{Login: "alice@example.com"}},
		Logf:     t.Logf,
		Handlers: map[string]StreamHandler{proto.StreamVideo: d.Video, proto.StreamInput: d.Input},
	}
	done := make(chan struct{})
	go func() { s.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	sess, err := viewer.Connect(ctx, tcpDial, ln.Addr().String(), viewer.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func TestVideoKeyframeThenDelta(t *testing.T) {
	scr := &fakeScreen{img: image.NewRGBA(image.Rect(0, 0, 128, 64))}
	sess := startDesktop(t, scr, &recInjector{})

	st, err := sess.Open(proto.StreamVideo)
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteJSON(st, proto.MsgVideoReq, proto.VideoReq{FPS: 30}); err != nil {
		t.Fatal(err)
	}
	var info proto.VideoInfo
	if err := proto.ReadJSON(st, proto.MsgVideoInfo, &info); err != nil {
		t.Fatal(err)
	}
	if len(info.Displays) != 1 || info.Displays[0].Width != 128 {
		t.Fatalf("info %+v", info)
	}
	st.SetReadDeadline(time.Now().Add(5 * time.Second))
	readTiles := func() int {
		typ, b, err := proto.ReadFrame(st)
		if err != nil || typ != proto.MsgVideoFrame {
			t.Fatalf("frame: type=%d err=%v", typ, err)
		}
		_, _, tl, err := tiles.Parse(b)
		if err != nil {
			t.Fatal(err)
		}
		return len(tl)
	}
	if n := readTiles(); n != 2 {
		t.Fatalf("keyframe tiles=%d want 2", n)
	}
	scr.mu.Lock()
	scr.img.SetRGBA(100, 10, color.RGBA{255, 255, 255, 255})
	scr.mu.Unlock()
	if n := readTiles(); n != 1 {
		t.Fatalf("delta tiles=%d want 1", n)
	}
}

func TestInputAppliedAndReleasedOnDisconnect(t *testing.T) {
	inj := &recInjector{}
	sess := startDesktop(t, &fakeScreen{img: image.NewRGBA(image.Rect(0, 0, 8, 8))}, inj)
	st, err := sess.Open(proto.StreamInput)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []input.Event{
		{T: "move", X: 0.5, Y: 0.5},
		{T: "keydown", Code: "ControlLeft"},
		{T: "down", Button: 0},
		{T: "keydown", Code: "MediaPlayPause"}, // unknown: ignored
	} {
		if err := proto.WriteJSON(st, proto.MsgInput, ev); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	st.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		ev, closed := inj.snapshot()
		if closed {
			want := []string{"move", "kdown", "down"}
			if len(ev) != 5 || ev[0] != want[0] || ev[1] != want[1] || ev[2] != want[2] {
				t.Fatalf("events %v", ev)
			}
			// last two are the releases (order of map iteration differs)
			if !(contains(ev[3:], "kup") && contains(ev[3:], "up")) {
				t.Fatalf("held inputs not released: %v", ev)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("injector not closed; events %v", ev)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

type fakeH264 struct{ forced int }

func (f *fakeH264) Encode(img *image.RGBA, fps, bitrate int, forceKey bool) ([]byte, bool, error) {
	if forceKey {
		f.forced++
		return []byte{0, 0, 0, 1, 0x65}, true, nil
	}
	return []byte{0, 0, 0, 1, 0x41}, false, nil
}
func (f *fakeH264) Close() {}

func videoSession(t *testing.T, newH264 func() (H264Encoder, error), codecs []string) (net.Conn, proto.VideoInfo, *fakeScreen) {
	t.Helper()
	scr := &fakeScreen{img: image.NewRGBA(image.Rect(0, 0, 101, 64))}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	d := &Desktop{Capturer: scr, NewH264: newH264, Logf: t.Logf}
	s := &Server{Policy: authz.Policy{AllowedLogins: []string{"alice@example.com"}}, Gate: autoGate(),
		Resolver: staticResolver{id: authz.Identity{Login: "alice@example.com"}}, Logf: t.Logf,
		Handlers: map[string]StreamHandler{proto.StreamVideo: d.Video}}
	done := make(chan struct{})
	go func() { s.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	sess, err := viewer.Connect(ctx, tcpDial, ln.Addr().String(), viewer.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	st, _ := sess.Open(proto.StreamVideo)
	st.SetDeadline(time.Now().Add(5 * time.Second))
	proto.WriteJSON(st, proto.MsgVideoReq, proto.VideoReq{FPS: 30, Codecs: codecs})
	var info proto.VideoInfo
	if err := proto.ReadJSON(st, proto.MsgVideoInfo, &info); err != nil {
		t.Fatal(err)
	}
	return st, info, scr
}

func TestVideoH264Negotiated(t *testing.T) {
	fe := &fakeH264{}
	st, info, scr := videoSession(t, func() (H264Encoder, error) { return fe, nil }, []string{"h264", "tiles"})
	if info.Codec != proto.CodecH264 {
		t.Fatalf("codec %q", info.Codec)
	}
	typ, b, err := proto.ReadFrame(st)
	if err != nil || typ != proto.MsgVideoH264 {
		t.Fatalf("frame type %d err %v", typ, err)
	}
	if b[0] != 1 || int(b[1])<<8|int(b[2]) != 100 || int(b[3])<<8|int(b[4]) != 64 {
		t.Fatalf("first frame header %v: want key, even-cropped 100x64", b[:5])
	}
	scr.mu.Lock()
	scr.img.Pix[0] = 9 // change the screen -> delta frame
	scr.mu.Unlock()
	if typ, b, _ := proto.ReadFrame(st); typ != proto.MsgVideoH264 || b[0] != 0 {
		t.Fatalf("second frame should be a delta, got type %d flags %d", typ, b[0])
	}
	// Re-sending a request forces a keyframe (viewer resync).
	proto.WriteJSON(st, proto.MsgVideoReq, proto.VideoReq{FPS: 30, Codecs: []string{"h264"}})
	for {
		typ, b, err := proto.ReadFrame(st)
		if err != nil {
			t.Fatal(err)
		}
		if typ == proto.MsgVideoH264 && b[0] == 1 {
			break
		}
	}
}

func TestVideoFallsBackToTiles(t *testing.T) {
	_, info, _ := videoSession(t, func() (H264Encoder, error) { return nil, errors.New("not installed") }, []string{"h264", "tiles"})
	if info.Codec != proto.CodecTiles {
		t.Fatalf("missing codec: got %q", info.Codec)
	}
	_, info, _ = videoSession(t, func() (H264Encoder, error) { return &fakeH264{}, nil }, []string{"tiles"})
	if info.Codec != proto.CodecTiles {
		t.Fatalf("viewer without h264: got %q", info.Codec)
	}
}
