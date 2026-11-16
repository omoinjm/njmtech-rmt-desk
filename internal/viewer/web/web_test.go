package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"image"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rmt-desk/internal/authz"
	"rmt-desk/internal/capture"
	"rmt-desk/internal/clipboard"
	"rmt-desk/internal/files"
	"rmt-desk/internal/host"
	"rmt-desk/internal/input"
	"rmt-desk/internal/proto"
	"rmt-desk/internal/tsnode"

	"github.com/coder/websocket"
)

type allowAll struct{}

func (allowAll) Resolve(context.Context, string) (authz.Identity, error) {
	return authz.Identity{Login: "alice@example.com", NodeName: "laptop"}, nil
}

type screen struct{}

func (screen) Displays() []capture.Display {
	return []capture.Display{{Index: 0, Bounds: image.Rect(0, 0, 100, 50), Width: 100, Height: 50}}
}
func (screen) Capture(int) (*image.RGBA, error) { return image.NewRGBA(image.Rect(0, 0, 100, 50)), nil }

type rec struct {
	mu sync.Mutex
	n  int
}

func (r *rec) inc()                               { r.mu.Lock(); r.n++; r.mu.Unlock() }
func (r *rec) MoveTo(int, float64, float64) error { r.inc(); return nil }
func (r *rec) Button(input.Button, bool) error    { r.inc(); return nil }
func (r *rec) Scroll(int, int) error              { r.inc(); return nil }
func (r *rec) Key(input.Key, bool) error          { r.inc(); return nil }
func (r *rec) Close() error                       { return nil }
func (r *rec) count() int                         { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

// Set once, before any Sync goroutine can read it.
func init() { clipboard.PollInterval = 20 * time.Millisecond }

type env struct {
	url, addr        string
	inj              *rec
	hostCB, viewerCB *clipboard.Memory
	hostDir          string
}

// setup starts a fake host (plain TCP) and the web bridge; returns the
// one-time URL, the bridge address, and the injector recorder.
func setup(t *testing.T) (string, string, *rec) {
	e := setupEnv(t)
	return e.url, e.addr, e.inj
}

func setupEnv(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e := &env{inj: &rec{}, hostCB: &clipboard.Memory{}, viewerCB: &clipboard.Memory{}, hostDir: t.TempDir()}
	inj := e.inj

	hl, _ := net.Listen("tcp", "127.0.0.1:0")
	desk := &host.Desktop{
		Capturer: screen{}, NewInjector: func() (input.Injector, error) { return inj, nil }, Logf: t.Logf,
		NewClipboard: func() (clipboard.Clipboard, error) { return e.hostCB, nil },
		Files:        &files.Exchange{Dir: e.hostDir},
	}
	hs := &host.Server{
		Policy: authz.Policy{AllowedLogins: []string{"alice@example.com"}},
		Gate:   autoGate(), Resolver: allowAll{}, Logf: t.Logf,
		Handlers: map[string]host.StreamHandler{
			proto.StreamVideo: desk.Video, proto.StreamInput: desk.Input,
			proto.StreamClipboard: desk.Clipboard, proto.StreamFiles: desk.FileStream,
		},
	}
	hdone := make(chan struct{})
	go func() { hs.Serve(ctx, hl); close(hdone) }()

	hostAddr := hl.Addr().String()
	ws := &Server{
		// The bridge dials "<host>:7070"; redirect to the fake host.
		Dial: func(ctx context.Context, n, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, n, hostAddr)
		},
		ListHosts: func(context.Context) ([]tsnode.Peer, error) {
			return []tsnode.Peer{{HostName: "rmt-box", Online: true}}, nil
		},
		NewClipboard: func() (clipboard.Clipboard, error) { return e.viewerCB, nil },
		Logf:         t.Logf,
	}
	ln, url, err := ws.Listen()
	if err != nil {
		t.Fatal(err)
	}
	wdone := make(chan struct{})
	go func() { ws.Serve(ctx, ln); close(wdone) }()
	t.Cleanup(func() { cancel(); <-hdone; <-wdone })
	e.url, e.addr = url, ln.Addr().String()
	return e
}

func client(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 5 * time.Second}
}

func TestAuthFlow(t *testing.T) {
	url, addr, _ := setup(t)
	base := "http://" + addr

	// No cookie: everything is 401.
	for _, p := range []string{"/", "/api/hosts", "/app.js"} {
		res, err := http.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without cookie: %d", p, res.StatusCode)
		}
	}
	// Wrong token.
	if res, _ := http.Get(base + "/?t=deadbeef"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", res.StatusCode)
	}

	c := client(t)
	res, err := c.Get(url)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("token login: %v %v", res.StatusCode, err)
	}
	res.Body.Close()
	if res.Request.URL.RawQuery != "" {
		t.Errorf("token should be stripped from URL, got %q", res.Request.URL)
	}
	res, err = c.Get(base + "/api/hosts")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("hosts: %v", err)
	}
	var peers []tsnode.Peer
	json.NewDecoder(res.Body).Decode(&peers)
	res.Body.Close()
	if len(peers) != 1 || peers[0].HostName != "rmt-box" {
		t.Fatalf("peers %+v", peers)
	}
}

func TestHostHeaderPinned(t *testing.T) {
	url, addr, _ := setup(t)
	req, _ := http.NewRequest("GET", url, nil)
	req.Host = "evil.example.com:" + strings.Split(addr, ":")[1] // DNS rebinding
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("rebinding host: %d", res.StatusCode)
	}
}

func TestBridgeEndToEnd(t *testing.T) {
	url, addr, inj := setup(t)
	c := client(t)
	if res, err := c.Get(url); err != nil {
		t.Fatal(err)
	} else {
		res.Body.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Cross-origin websocket must be refused.
	_, _, err := websocket.Dial(ctx, "ws://"+addr+"/ws?host=rmt-box", &websocket.DialOptions{
		HTTPClient: c, HTTPHeader: http.Header{"Origin": {"http://evil.example.com"}},
	})
	if err == nil {
		t.Fatal("cross-origin websocket accepted")
	}

	wc, _, err := websocket.Dial(ctx, "ws://"+addr+"/ws?host=rmt-box", &websocket.DialOptions{
		HTTPClient: c, HTTPHeader: http.Header{"Origin": {"http://" + addr}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer wc.CloseNow()

	readText := func(want string) map[string]any {
		for {
			typ, b, err := wc.Read(ctx)
			if err != nil {
				t.Fatalf("waiting for %s: %v", want, err)
			}
			if typ != websocket.MessageText {
				continue
			}
			var m map[string]any
			json.Unmarshal(b, &m)
			if m["type"] == want {
				return m
			}
		}
	}
	wc.Write(ctx, websocket.MessageText, []byte(`{"type":"connect","data":{}}`))
	readText("hello")
	wc.Write(ctx, websocket.MessageText, []byte(`{"type":"video","data":{"display":0,"fps":30,"quality":50}}`))
	info := readText("info")
	if d := info["data"].(map[string]any)["displays"].([]any); len(d) != 1 {
		t.Fatalf("info %v", info)
	}
	// First binary message is a keyframe for a 100x50 screen.
	for {
		typ, b, err := wc.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.MessageBinary {
			if b[0] != 0 {
				t.Fatalf("codec tag %d, want 0 (tiles)", b[0])
			}
			if w, h := int(b[1])<<8|int(b[2]), int(b[3])<<8|int(b[4]); w != 100 || h != 50 {
				t.Fatalf("frame %dx%d", w, h)
			}
			break
		}
	}
	// Input reaches the injector.
	wc.Write(ctx, websocket.MessageText, []byte(`{"type":"input","data":{"t":"move","x":0.5,"y":0.5}}`))
	wc.Write(ctx, websocket.MessageText, []byte(`{"type":"input","data":{"t":"keydown","code":"KeyA"}}`))
	deadline := time.Now().Add(3 * time.Second)
	for inj.count() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("injector got %d events, want 2", inj.count())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFilesAndClipboardThroughBridge(t *testing.T) {
	e := setupEnv(t)
	c := client(t)
	if res, err := c.Get(e.url); err != nil {
		t.Fatal(err)
	} else {
		res.Body.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wc, _, err := websocket.Dial(ctx, "ws://"+e.addr+"/ws?host=rmt-box", &websocket.DialOptions{
		HTTPClient: c, HTTPHeader: http.Header{"Origin": {"http://" + e.addr}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer wc.CloseNow()
	wc.Write(ctx, websocket.MessageText, []byte(`{"type":"connect","data":{}}`))
	var sid string
	for sid == "" {
		_, b, err := wc.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Type string
			Data struct{ SID string }
		}
		json.Unmarshal(b, &m)
		if m.Type == "hello" {
			sid = m.Data.SID
		}
	}
	// Keep draining the websocket so the bridge never blocks.
	go func() {
		for {
			if _, _, err := wc.Read(ctx); err != nil {
				return
			}
		}
	}()
	base := "http://" + e.addr

	// Clipboard: viewer -> host and host -> viewer (bridge uses its own local clipboard).
	time.Sleep(100 * time.Millisecond)
	e.viewerCB.Write("from laptop")
	waitFor(t, func() bool { v, _ := e.hostCB.Read(); return v == "from laptop" })
	e.hostCB.Write("from host")
	waitFor(t, func() bool { v, _ := e.viewerCB.Read(); return v == "from host" })

	// Upload without the anti-CSRF header is refused.
	data := make([]byte, 700_000)
	rand.Read(data)
	res, _ := c.Post(base+"/api/upload?sid="+sid+"&name=../x.bin", "application/octet-stream", bytes.NewReader(data))
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("upload without header: %d", res.StatusCode)
	}
	// Unknown session id.
	res, _ = c.Get(base + "/api/files?sid=nope")
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("bad sid: %d", res.StatusCode)
	}

	req, _ := http.NewRequest("POST", base+"/api/upload?sid="+sid+"&name=../x.bin", bytes.NewReader(data))
	req.Header.Set("X-Rmt-Upload", "1")
	res, err = c.Do(req)
	if err != nil || res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("upload: %v %d %s", err, res.StatusCode, b)
	}
	res.Body.Close()
	if b, _ := os.ReadFile(filepath.Join(e.hostDir, "x.bin")); !bytes.Equal(b, data) {
		t.Fatal("uploaded file missing or corrupted on host")
	}

	res, _ = c.Get(base + "/api/files?sid=" + sid)
	var l proto.FileList
	json.NewDecoder(res.Body).Decode(&l)
	res.Body.Close()
	if len(l.Files) != 1 || l.Files[0].Name != "x.bin" {
		t.Fatalf("list: %+v", l)
	}

	res, err = c.Get(base + "/api/download?sid=" + sid + "&name=x.bin")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("download: %v", err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !bytes.Equal(got, data) || !strings.Contains(res.Header.Get("Content-Disposition"), `filename=x.bin`) {
		t.Fatalf("download mismatch (%d bytes, cd=%q)", len(got), res.Header.Get("Content-Disposition"))
	}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
