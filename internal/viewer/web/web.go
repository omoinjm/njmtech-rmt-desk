// Package web is the viewer's local UI: an HTTP server bound to 127.0.0.1
// that serves a single-page app and bridges a browser websocket to an rmt
// session over the tailnet.
//
// Local-only hardening: random per-run token exchanged for an HttpOnly,
// SameSite=Strict cookie; Host header pinned to the loopback address (DNS
// rebinding); websocket same-origin check.
//
// Embedded mode (the rmt-desk app): the UI is served by the app's web view,
// so the page is cross-origin to this server. Only the session websocket is
// used, authenticated by the same per-run token (sent as a websocket
// subprotocol, which the app hands to the page through a native binding)
// and an exact match on one of EmbedOrigins. Everything else goes through
// the exported methods, called in-process.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"rmt-desk/internal/clipboard"
	"rmt-desk/internal/files"
	"rmt-desk/internal/proto"
	"rmt-desk/internal/tsnode"
	"rmt-desk/internal/viewer"
	"rmt-desk/internal/viewer/bridge"

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
	// EmbedOrigins enables embedded mode for these exact page origins
	// (e.g. "wails://wails"). Empty: browser mode only.
	EmbedOrigins []string

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
	return s.lookup(r.URL.Query().Get("sid"))
}

func (s *Server) lookup(sid string) (*viewer.Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sid]
	return sess, ok
}

// WSURL is the session websocket URL and Subprotocol the token-bearing
// subprotocol an embedded page must offer. Valid after Listen.
func (s *Server) WSURL() string       { return "ws://" + s.addr + "/ws" }
func (s *Server) Subprotocol() string { return "rmt." + s.token }

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
	mux.Handle("GET /{file}", s.auth(http.HandlerFunc(s.asset))) // scripts and styles
	mux.Handle("GET /api/hosts", s.auth(http.HandlerFunc(s.hosts)))
	mux.HandleFunc("GET /ws", s.ws) // authenticates itself (cookie or embedded token)
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
			"default-src 'self'; img-src 'self' blob: data:; connect-src 'self' ws://"+s.addr+"; frame-ancestors 'none'")
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
	name := r.PathValue("file")
	ct := map[string]string{".js": "text/javascript", ".css": "text/css"}[path.Ext(name)]
	b, err := static.ReadFile("static/" + name)
	if ct == "" || err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct+"; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
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

var errUnauthorized = errors.New("unauthorized")

// acceptWS authenticates and upgrades: browser mode (cookie + same-origin)
// or embedded mode (exact origin + token subprotocol).
func (s *Server) acceptWS(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	if s.authed(r) {
		return websocket.Accept(w, r, nil) // nil opts => same-origin enforced
	}
	if s.embedAuthed(r) {
		return websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true, // origin checked exactly by embedAuthed
			Subprotocols:       []string{s.Subprotocol()},
		})
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return nil, errUnauthorized
}

func (s *Server) embedAuthed(r *http.Request) bool {
	if len(s.EmbedOrigins) == 0 || !slices.Contains(s.EmbedOrigins, r.Header.Get("Origin")) {
		return false
	}
	want := []byte(s.Subprotocol())
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(h, ",") {
			if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(p)), want) == 1 {
				return true
			}
		}
	}
	return false
}

func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	if host == "" {
		http.Error(w, "missing host", http.StatusBadRequest)
		return
	}
	wc, err := s.acceptWS(w, r)
	if err != nil {
		return
	}
	defer wc.CloseNow()
	wc.SetReadLimit(64 << 10)
	bridge.Run(r.Context(), bridge.Config{
		Dial: s.Dial, NewClipboard: s.NewClipboard, Logf: s.logf,
		AddSession: s.addSession, DropSession: s.dropSession,
	}, host, wc)
}

// ---------- file transfer (exchange folder on the host) ----------

// ErrNoSession: sid doesn't name a live session.
var ErrNoSession = errors.New("no such session")

// ListFiles lists the host's exchange folder for session sid.
func (s *Server) ListFiles(sid string) (proto.FileList, error) {
	sess, ok := s.lookup(sid)
	if !ok {
		return proto.FileList{}, ErrNoSession
	}
	st, err := sess.Open(proto.StreamFiles)
	if err != nil {
		return proto.FileList{}, err
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(15 * time.Second))
	l, err := files.List(st)
	if err != nil {
		return l, errors.New(remoteMsg(err))
	}
	return l, nil
}

// UploadFile sends size bytes from r to the host's exchange folder as name.
// Cancelling ctx aborts the transfer.
func (s *Server) UploadFile(ctx context.Context, sid, name string, size int64, r io.Reader) (proto.FileDone, error) {
	sess, ok := s.lookup(sid)
	if !ok {
		return proto.FileDone{}, ErrNoSession
	}
	st, err := sess.Open(proto.StreamFiles)
	if err != nil {
		return proto.FileDone{}, err
	}
	defer st.Close()
	stop := context.AfterFunc(ctx, func() { st.Close() })
	defer stop()
	done, err := files.Put(st, name, size, r)
	if err != nil {
		return done, errors.New(remoteMsg(err))
	}
	s.logf("uploaded %s (%d bytes, sha256 %s)", done.Name, done.Size, done.SHA256[:12])
	return done, nil
}

// DownloadFile streams the host file name into w (onStart gets its size
// first). A checksum mismatch at the end returns an error: discard w.
func (s *Server) DownloadFile(ctx context.Context, sid, name string, onStart func(proto.FileAck), w io.Writer) error {
	sess, ok := s.lookup(sid)
	if !ok {
		return ErrNoSession
	}
	st, err := sess.Open(proto.StreamFiles)
	if err != nil {
		return err
	}
	defer st.Close()
	stop := context.AfterFunc(ctx, func() { st.Close() })
	defer stop()
	if _, err := files.Get(st, name, onStart, w); err != nil {
		return errors.New(remoteMsg(err))
	}
	return nil
}

func (s *Server) fileList(w http.ResponseWriter, r *http.Request) {
	l, err := s.ListFiles(r.URL.Query().Get("sid"))
	if errors.Is(err, ErrNoSession) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
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

// Assets is the UI (index.html, scripts, styles) for embedding in the app.
func Assets() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
