// Package bridge connects a message-oriented client (the viewer UI's
// websocket, or an in-page channel in the WebAssembly build) to an rmt
// session: it performs the handshake, then pumps video to the client and
// input/requests to the host. It doesn't depend on tsnet, so it builds for
// js/wasm.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"rmt-desk/internal/clipboard"
	"rmt-desk/internal/config"
	"rmt-desk/internal/proto"
	"rmt-desk/internal/viewer"

	"github.com/coder/websocket"
)

// Conn is the client side: *websocket.Conn, or anything with its semantics.
type Conn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Close(code websocket.StatusCode, reason string) error
}

type Config struct {
	Dial viewer.Dialer
	// NewClipboard opens this machine's clipboard (nil disables sync).
	NewClipboard func() (clipboard.Clipboard, error)
	Logf         func(string, ...any)
	// AddSession/DropSession register the live session (e.g. for file
	// transfer endpoints); optional.
	AddSession  func(*viewer.Session) string
	DropSession func(sid string)
}

func (c Config) logf(f string, a ...any) {
	if c.Logf != nil {
		c.Logf(f, a...)
	}
}

func (c Config) addSession(s *viewer.Session) string {
	if c.AddSession == nil {
		return ""
	}
	return c.AddSession(s)
}

func (c Config) dropSession(sid string) {
	if c.DropSession != nil {
		c.DropSession(sid)
	}
}

// Browser <-> bridge messages (text frames, JSON). Video tiles go to the
// browser as binary frames verbatim (tiles wire format).
type Msg struct {
	Type  string          `json:"type"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
	Code  string          `json:"code,omitempty"`
}

// connectReq is the browser's first message; credentials travel over the
// websocket, never in a URL.
type connectReq struct {
	Password string `json:"password"`
	TOTP     string `json:"totp"`
}

// Run serves one client until either side closes. host is a tailnet name or
// IP; the client's first message must be {"type":"connect"}.
func Run(ctx context.Context, cfg Config, host string, wc Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	send := func(m Msg) error {
		b, _ := json.Marshal(m)
		return wc.Write(ctx, websocket.MessageText, b)
	}
	fail := func(err error) {
		m := Msg{Type: "error", Error: err.Error()}
		if re, ok := err.(*proto.RemoteError); ok {
			m.Error, m.Code = re.Err.Message, re.Err.Code
		}
		_ = send(m)
		wc.Close(websocket.StatusNormalClosure, "")
	}

	var creq connectReq
	rctx, rcancel := context.WithTimeout(ctx, 10*time.Second)
	_, first, err := wc.Read(rctx)
	rcancel()
	var m0 Msg
	if err != nil || json.Unmarshal(first, &m0) != nil || m0.Type != "connect" {
		return
	}
	_ = json.Unmarshal(m0.Data, &creq)

	_ = send(Msg{Type: "status", Error: "connecting… (the host may be asking its user for permission)"})
	dctx, dcancel := context.WithTimeout(ctx, 100*time.Second)
	sess, err := viewer.Connect(dctx, cfg.Dial, net.JoinHostPort(host, strconv.Itoa(config.DefaultPort)),
		viewer.Auth{Password: creq.Password, TOTP: creq.TOTP})
	dcancel()
	if err != nil {
		fail(err)
		return
	}
	defer sess.Close()
	context.AfterFunc(ctx, func() { sess.Close() })
	cfg.logf("connected to %s (%s)", sess.Info.Host, sess.Info.OS)
	sid := cfg.addSession(sess)
	defer cfg.dropSession(sid)
	info, _ := json.Marshal(struct {
		proto.HelloAck
		SID string `json:"sid"`
	}{sess.Info, sid})
	if err := send(Msg{Type: "hello", Data: info}); err != nil {
		return
	}

	// Clipboard sync between this machine and the host (if granted).
	var clipCancel context.CancelFunc
	setClipboard := func(on bool) {
		if clipCancel != nil {
			clipCancel()
			clipCancel = nil
		}
		if !on || cfg.NewClipboard == nil || !slices.Contains(sess.Info.Granted, "clipboard") {
			return
		}
		cb, err := cfg.NewClipboard()
		if err != nil {
			_ = send(Msg{Type: "clipboard", Error: "local clipboard unavailable: " + err.Error()})
			return
		}
		st, err := sess.Open(proto.StreamClipboard)
		if err != nil {
			return
		}
		cctx, ccancel := context.WithCancel(ctx)
		clipCancel = ccancel
		go func() {
			err := clipboard.Sync(cctx, cb, st, cfg.logf)
			if cctx.Err() == nil {
				var re *proto.RemoteError
				if errors.As(err, &re) {
					_ = send(Msg{Type: "clipboard", Error: "host clipboard unavailable: " + re.Err.Message})
				}
			}
		}()
	}
	setClipboard(true)
	defer func() { setClipboard(false) }()

	video, err := sess.Open(proto.StreamVideo)
	if err != nil {
		fail(err)
		return
	}
	// Host -> browser video pump.
	go func() {
		defer cancel()
		for {
			t, b, err := proto.ReadFrame(video)
			if err != nil {
				_ = send(Msg{Type: "error", Error: "session closed"})
				return
			}
			switch t {
			case proto.MsgVideoFrame, proto.MsgVideoH264:
				// Binary websocket messages carry a 1-byte codec tag.
				tag := byte(0)
				if t == proto.MsgVideoH264 {
					tag = 1
				}
				if wc.Write(ctx, websocket.MessageBinary, append([]byte{tag}, b...)) != nil {
					return
				}
			case proto.MsgVideoInfo:
				_ = send(Msg{Type: "info", Data: b})
			case proto.MsgError:
				var e proto.Error
				_ = json.Unmarshal(b, &e)
				_ = send(Msg{Type: "error", Error: e.Message})
				return
			}
		}
	}()

	// Input is optional: the host may refuse (no permission / another
	// controller / injection unavailable) and we stay view-only.
	var inputOK atomic.Bool
	in, err := sess.Open(proto.StreamInput)
	if err == nil {
		inputOK.Store(true)
		go func() {
			// The host only ever writes an Error on this stream.
			var e proto.Error
			if proto.ReadJSON(in, proto.MsgError, &e) == nil {
				inputOK.Store(false)
				_ = send(Msg{Type: "view-only", Error: e.Message})
			}
		}()
	}

	// Browser -> host.
	for {
		_, b, err := wc.Read(ctx)
		if err != nil {
			return
		}
		var m Msg
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		switch m.Type {
		case "video":
			var req proto.VideoReq
			if json.Unmarshal(m.Data, &req) == nil && proto.WriteJSON(video, proto.MsgVideoReq, req) != nil {
				return
			}
		case "input":
			if inputOK.Load() && proto.WriteFrame(in, proto.MsgInput, m.Data) != nil {
				inputOK.Store(false)
			}
		case "clipboard":
			var c struct {
				Enabled bool `json:"enabled"`
			}
			if json.Unmarshal(m.Data, &c) == nil {
				setClipboard(c.Enabled)
			}
		}
	}
}
