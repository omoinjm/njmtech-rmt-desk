package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"rmt-desk/internal/proto"

	"github.com/coder/websocket"
)

func dialEmbedded(ctx context.Context, e *env, origin, subproto string) (*websocket.Conn, error) {
	opts := &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {origin}}}
	if subproto != "" {
		opts.Subprotocols = []string{subproto}
	}
	wc, _, err := websocket.Dial(ctx, e.srv.WSURL()+"?host=rmt-box", opts)
	return wc, err
}

func TestEmbeddedWebsocketAuth(t *testing.T) {
	e := setupEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, c := range []struct{ name, origin, proto string }{
		{"no token", "wails://wails", ""},
		{"wrong token", "wails://wails", "rmt.deadbeef"},
		{"wrong origin", "http://evil.example.com", e.srv.Subprotocol()},
		{"no origin match by prefix", "wails://wails.evil", e.srv.Subprotocol()},
	} {
		if wc, err := dialEmbedded(ctx, e, c.origin, c.proto); err == nil {
			wc.CloseNow()
			t.Errorf("%s: accepted", c.name)
		}
	}

	wc, err := dialEmbedded(ctx, e, "wails://wails", e.srv.Subprotocol())
	if err != nil {
		t.Fatal(err)
	}
	defer wc.CloseNow()
	if wc.Subprotocol() != e.srv.Subprotocol() {
		t.Fatalf("subprotocol not echoed: %q", wc.Subprotocol())
	}
	wc.Write(ctx, websocket.MessageText, []byte(`{"type":"connect","data":{}}`))
	var sid string
	for sid == "" {
		_, b, err := wc.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var m msg
		json.Unmarshal(b, &m)
		if m.Type == "hello" {
			var h struct {
				SID string `json:"sid"`
			}
			json.Unmarshal(m.Data, &h)
			sid = h.SID
		}
	}

	// In-process file operations (what the app's bindings call).
	data := bytes.Repeat([]byte("rmt"), 10000)
	if _, err := e.srv.UploadFile(ctx, sid, "a.bin", int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(e.hostDir, "a.bin")); !bytes.Equal(got, data) {
		t.Fatal("uploaded content mismatch")
	}
	l, err := e.srv.ListFiles(sid)
	if err != nil || len(l.Files) != 1 {
		t.Fatalf("list: %+v %v", l, err)
	}
	var buf bytes.Buffer
	var size int64
	if err := e.srv.DownloadFile(ctx, sid, "a.bin", func(a proto.FileAck) { size = a.Size }, &buf); err != nil {
		t.Fatal(err)
	}
	if size != int64(len(data)) || !bytes.Equal(buf.Bytes(), data) {
		t.Fatal("download mismatch")
	}
	if _, err := e.srv.ListFiles("nope"); err != ErrNoSession {
		t.Fatalf("unknown sid: %v", err)
	}
}
