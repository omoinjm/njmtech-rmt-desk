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
	"errors"
	"log"
	"mime"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"rmt-desk/internal/clipboard"
	"rmt-desk/internal/config"
	"rmt-desk/internal/files"
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
	// NewClipboard opens the viewer machine's clipboard (nil disables sync).
	NewClipboard func() (clipboard.Clipboard, error)
	Logf         func(string, ...any)

	token string
	addr  string

	mu       sync.Mutex
	sessions map[string]*viewer.Session // per browser tab, for file endpoints
}

func (s *Server) addSession(sess *viewer.Session) string {
	b := make([]byte, 16)
	rand.Read(b)
	id := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]*viewer.Session{}
	}
	s.sessions[id] = sess
	return id
}

func (s *Server) dropSession(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

func (s *Server) session(r *http.Request) (*viewer.Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[r.URL.Query().Get("sid")]
	return sess, ok
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
	mux.Handle("GET /api/files", s.auth(http.HandlerFunc(s.fileList)))
	mux.Handle("GET /api/download", s.auth(http.HandlerFunc(s.download)))
	mux.Handle("POST /api/upload", s.auth(http.HandlerFunc(s.upload)))
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
	Code  string          `json:"code,omitempty"`
}

// connectReq is the browser's first message; credentials travel over the
// websocket, never in a URL.
type connectReq struct {
	Password string `json:"password"`
	TOTP     string `json:"totp"`
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
		m := msg{Type: "error", Error: err.Error()}
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
	var m0 msg
	if err != nil || json.Unmarshal(first, &m0) != nil || m0.Type != "connect" {
		return
	}
	_ = json.Unmarshal(m0.Data, &creq)

	_ = send(msg{Type: "status", Error: "connecting… (the host may be asking its user for permission)"})
	dctx, dcancel := context.WithTimeout(ctx, 100*time.Second)
	sess, err := viewer.Connect(dctx, s.Dial, net.JoinHostPort(host, strconv.Itoa(config.DefaultPort)),
		viewer.Auth{Password: creq.Password, TOTP: creq.TOTP})
	dcancel()
	if err != nil {
		fail(err)
		return
	}
	defer sess.Close()
	context.AfterFunc(ctx, func() { sess.Close() })
	s.logf("connected to %s (%s)", sess.Info.Host, sess.Info.OS)
	sid := s.addSession(sess)
	defer s.dropSession(sid)
	info, _ := json.Marshal(struct {
		proto.HelloAck
		SID string `json:"sid"`
	}{sess.Info, sid})
	if err := send(msg{Type: "hello", Data: info}); err != nil {
		return
	}

	// Clipboard sync between this machine and the host (if granted).
	var clipCancel context.CancelFunc
	setClipboard := func(on bool) {
		if clipCancel != nil {
			clipCancel()
			clipCancel = nil
		}
		if !on || s.NewClipboard == nil || !slices.Contains(sess.Info.Granted, "clipboard") {
			return
		}
		cb, err := s.NewClipboard()
		if err != nil {
			_ = send(msg{Type: "clipboard", Error: "local clipboard unavailable: " + err.Error()})
			return
		}
		st, err := sess.Open(proto.StreamClipboard)
		if err != nil {
			return
		}
		cctx, ccancel := context.WithCancel(ctx)
		clipCancel = ccancel
		go func() {
			err := clipboard.Sync(cctx, cb, st, s.logf)
			if cctx.Err() == nil {
				var re *proto.RemoteError
				if errors.As(err, &re) {
					_ = send(msg{Type: "clipboard", Error: "host clipboard unavailable: " + re.Err.Message})
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

// ---------- file transfer (exchange folder on the host) ----------

func (s *Server) fileList(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(r)
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	st, err := sess.Open(proto.StreamFiles)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(15 * time.Second))
	l, err := files.List(st)
	if err != nil {
		http.Error(w, remoteMsg(err), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(l)
}

// upload streams the request body to the host. It requires a custom header,
// which a cross-site form can't send without a CORS preflight (we never
// answer preflights), on top of the SameSite cookie.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Rmt-Upload") != "1" {
		http.Error(w, "missing header", http.StatusForbidden)
		return
	}
	sess, ok := s.session(r)
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	if r.ContentLength < 0 {
		http.Error(w, "Content-Length required", http.StatusLengthRequired)
		return
	}
	st, err := sess.Open(proto.StreamFiles)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer st.Close()
	context.AfterFunc(r.Context(), func() { st.Close() }) // browser cancelled
	done, err := files.Put(st, r.URL.Query().Get("name"), r.ContentLength, r.Body)
	if err != nil {
		http.Error(w, remoteMsg(err), http.StatusBadGateway)
		return
	}
	s.logf("uploaded %s (%d bytes, sha256 %s)", done.Name, done.Size, done.SHA256[:12])
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(done)
}

// download streams a host file to the browser, which saves it natively.
// If the checksum fails at the end, the response is aborted so the browser
// marks the download as failed instead of keeping a corrupt file.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(r)
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	st, err := sess.Open(proto.StreamFiles)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer st.Close()
	context.AfterFunc(r.Context(), func() { st.Close() })
	started := false
	_, err = files.Get(st, r.URL.Query().Get("name"), func(a proto.FileAck) {
		started = true
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(a.Size, 10))
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.Name}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
	}, w)
	if err != nil {
		if !started {
			http.Error(w, remoteMsg(err), http.StatusBadGateway)
			return
		}
		s.logf("download failed: %v", err)
		panic(http.ErrAbortHandler) // reset the connection: browser shows "failed"
	}
}

func remoteMsg(err error) string {
	var re *proto.RemoteError
	if errors.As(err, &re) {
		return re.Err.Message
	}
	return err.Error()
}
