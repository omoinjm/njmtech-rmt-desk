// Package web is the viewer's local UI: an HTTP server bound to 127.0.0.1
// that serves a single-page app and bridges a browser websocket to an rmt
// session over the tailnet.
//
// Local-only hardening: random per-run token exchanged for an HttpOnly,
// SameSite=Strict cookie; Host header pinned to the loopback address (DNS
// rebinding); websocket same-origin check.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"rmt-desk/internal/config"
	"rmt-desk/internal/proto"
	"rmt-desk/internal/tsnode"
	"rmt-desk/internal/viewer"

	"github.com/coder/websocket"
)

//go:embed static
var static embed.FS

const cookieName = "rmt_session"

type Server struct {
	Dial      viewer.Dialer
	ListHosts func(ctx context.Context) ([]tsnode.Peer, error)
	Logf      func(string, ...any)

	token string
	addr  string
}

func (s *Server) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	} else {
		log.Printf(f, a...)
	}
}

// Listen binds a random loopback port and returns the one-time URL to open.
func (s *Server) Listen() (net.Listener, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, "", err
	}
	s.token = hex.EncodeToString(b)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	s.addr = ln.Addr().String()
	return ln, "http://" + s.addr + "/?t=" + s.token, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.Handle("GET /app.js", s.auth(http.HandlerFunc(s.asset)))
	mux.Handle("GET /app.css", s.auth(http.HandlerFunc(s.asset)))
	mux.Handle("GET /api/hosts", s.auth(http.HandlerFunc(s.hosts)))
	mux.Handle("GET /ws", s.auth(http.HandlerFunc(s.ws)))
	return s.hostCheck(mux)
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	context.AfterFunc(ctx, func() { srv.Close() })
	err := srv.Serve(ln)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (s *Server) hostCheck(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != s.addr {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' blob:; connect-src 'self' ws://"+s.addr+"; frame-ancestors 'none'")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.token)) == 1
}

func (s *Server) auth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if t := r.URL.Query().Get("t"); t != "" {
		if subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: cookieName, Value: s.token, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, "/", http.StatusSeeOther) // drop token from URL/history
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized: open the URL printed by `rmt ui`", http.StatusUnauthorized)
		return
	}
	b, _ := static.ReadFile("static/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[1:]
	b, err := static.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := map[string]string{"app.js": "text/javascript", "app.css": "text/css"}[name]
	w.Header().Set("Content-Type", ct+"; charset=utf-8")
	w.Write(b)
}

func (s *Server) hosts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	peers, err := s.ListHosts(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if peers == nil {
		peers = []tsnode.Peer{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(peers)
}

// Browser <-> bridge messages (text frames, JSON). Video tiles go to the
// browser as binary frames verbatim (tiles wire format).
type msg struct {
	Type  string          `json:"type"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	if host == "" {
		http.Error(w, "missing host", http.StatusBadRequest)
		return
	}
	wc, err := websocket.Accept(w, r, nil) // nil opts => same-origin enforced
	if err != nil {
		return
	}
	defer wc.CloseNow()
	wc.SetReadLimit(64 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	send := func(m msg) error {
		b, _ := json.Marshal(m)
		return wc.Write(ctx, websocket.MessageText, b)
	}
	fail := func(err error) {
		_ = send(msg{Type: "error", Error: err.Error()})
		wc.Close(websocket.StatusNormalClosure, "")
	}

	dctx, dcancel := context.WithTimeout(ctx, 20*time.Second)
	sess, err := viewer.Connect(dctx, s.Dial, net.JoinHostPort(host, strconv.Itoa(config.DefaultPort)), "")
	dcancel()
	if err != nil {
		fail(err)
		return
	}
	defer sess.Close()
	context.AfterFunc(ctx, func() { sess.Close() })
	s.logf("connected to %s (%s)", sess.Info.Host, sess.Info.OS)
	info, _ := json.Marshal(sess.Info)
	if err := send(msg{Type: "hello", Data: info}); err != nil {
		return
	}

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
				_ = send(msg{Type: "error", Error: "session closed"})
				return
			}
			switch t {
			case proto.MsgVideoFrame:
				if wc.Write(ctx, websocket.MessageBinary, b) != nil {
					return
				}
			case proto.MsgVideoInfo:
				_ = send(msg{Type: "info", Data: b})
			case proto.MsgError:
				var e proto.Error
				_ = json.Unmarshal(b, &e)
				_ = send(msg{Type: "error", Error: e.Message})
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
				_ = send(msg{Type: "view-only", Error: e.Message})
			}
		}()
	}

	// Browser -> host.
	for {
		_, b, err := wc.Read(ctx)
		if err != nil {
			return
		}
		var m msg
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
		}
	}
}
