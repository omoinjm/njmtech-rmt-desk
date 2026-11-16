package web

import (
	"context"
	"encoding/json"
	"image"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"testing"
	"time"

	"rmt-desk/internal/authz"
	"rmt-desk/internal/capture"
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

// setup starts a fake host (plain TCP) and the web bridge; returns the
// one-time URL, the bridge address, and the injector recorder.
func setup(t *testing.T) (string, string, *rec) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	inj := &rec{}

	hl, _ := net.Listen("tcp", "127.0.0.1:0")
	desk := &host.Desktop{Capturer: screen{}, NewInjector: func() (input.Injector, error) { return inj, nil }, Logf: t.Logf}
	hs := &host.Server{
		Policy: authz.Policy{AllowedLogins: []string{"alice@example.com"}}, Resolver: allowAll{}, Logf: t.Logf,
		Handlers: map[string]host.StreamHandler{proto.StreamVideo: desk.Video, proto.StreamInput: desk.Input},
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
		Logf: t.Logf,
	}
	ln, url, err := ws.Listen()
	if err != nil {
		t.Fatal(err)
	}
	wdone := make(chan struct{})
	go func() { ws.Serve(ctx, ln); close(wdone) }()
	t.Cleanup(func() { cancel(); <-hdone; <-wdone })
	return url, ln.Addr().String(), inj
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
			if w, h := int(b[0])<<8|int(b[1]), int(b[2])<<8|int(b[3]); w != 100 || h != 50 {
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
