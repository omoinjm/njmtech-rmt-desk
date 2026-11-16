package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"rmt-desk/internal/access"
	"rmt-desk/internal/clipboard"
	"rmt-desk/internal/config"
	"rmt-desk/internal/hostctl"
	"rmt-desk/internal/hostsetup"
	"rmt-desk/internal/proto"
	"rmt-desk/internal/tsnode"
	"rmt-desk/internal/viewer/web"

	"github.com/wailsapp/wails/v2/pkg/options"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"rsc.io/qr"
)

// Page origins of the Wails web view (Windows / macOS + Linux).
var embedOrigins = []string{"http://wails.localhost", "wails://wails"}

// App is bound to the frontend: every exported method is callable from JS
// as window.go.main.App.<Method>.
type App struct {
	ctx       context.Context
	setupMode bool
	tray      bool // icon in the system tray; the window hides on close
	prefill   string
	hostDir   string

	// main mode
	node   *tsnode.Node
	bridge *web.Server
	stop   context.CancelFunc

	// setup wizard
	loginMu     sync.Mutex
	loginCancel context.CancelFunc
}

func newApp(setupMode bool, prefill string) *App {
	return &App{setupMode: setupMode, prefill: prefill, hostDir: hostsetup.StateDir()}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if a.setupMode {
		return
	}
	if a.tray {
		a.startTray()
	}
	hn, _ := os.Hostname()
	node, err := tsnode.Open(tsnode.Options{
		Hostname: "rmt-viewer-" + hn,
		StateDir: tsnode.DefaultStateDir("viewer"),
		PerUser:  true,
	})
	if err != nil {
		log.Printf("viewer node: %v", err)
		return
	}
	a.node = node
	a.bridge = &web.Server{
		Dial:         node.Srv.Dial,
		ListHosts:    func(ctx context.Context) ([]tsnode.Peer, error) { return node.Hosts(ctx, config.HostTag) },
		NewClipboard: clipboard.New,
		EmbedOrigins: embedOrigins,
	}
	ln, _, err := a.bridge.Listen()
	if err != nil {
		log.Printf("bridge: %v", err)
		return
	}
	bctx, cancel := context.WithCancel(context.Background())
	a.stop = cancel
	go a.bridge.Serve(bctx, ln)
}

func (a *App) shutdown(context.Context) {
	if a.tray {
		a.stopTray()
	}
	a.loginMu.Lock()
	if a.loginCancel != nil {
		a.loginCancel()
	}
	a.loginMu.Unlock()
	if a.stop != nil {
		a.stop()
	}
	if a.node != nil {
		a.node.Close()
	}
}

func (a *App) onSecondInstance(options.SecondInstanceData) {
	wruntime.WindowUnminimise(a.ctx)
	wruntime.Show(a.ctx)
	if a.tray {
		hideTaskbarButton()
	}
}

// domReady: the window is up; a tray app has no taskbar button.
func (a *App) domReady(context.Context) {
	if a.tray {
		hideTaskbarButton()
	}
}

// ---------- general ----------

// Info tells the frontend how it is running.
type Info struct {
	Mode    string `json:"mode"` // "main" | "setup"
	OS      string `json:"os"`
	Version string `json:"version"`
	// HostSupported is false where this build can't act as a host
	// (e.g. an AppImage, whose path changes on every run).
	HostSupported bool   `json:"hostSupported"`
	HostNote      string `json:"hostNote,omitempty"`
}

func (a *App) Info() Info {
	in := Info{Mode: "main", OS: runtime.GOOS, Version: version, HostSupported: true}
	if a.setupMode {
		in.Mode = "setup"
	}
	if os.Getenv("APPIMAGE") != "" {
		in.HostSupported, in.HostNote = false, "The AppImage can connect to other devices only. Install the .deb or .rpm package to allow remote access to this computer."
	} else if _, err := hostsetup.RmtdPath(); err != nil {
		in.HostSupported, in.HostNote = false, "rmtd is missing from the installation; reinstall rmt-desk to allow remote access to this computer."
	}
	return in
}

// OpenURL opens an http(s) link in the system browser.
func (a *App) OpenURL(u string) error {
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return errors.New("only web links can be opened")
	}
	wruntime.BrowserOpenURL(a.ctx, u)
	return nil
}

// CopyText puts s on this computer's clipboard.
func (a *App) CopyText(s string) error { return wruntime.ClipboardSetText(a.ctx, s) }

// ---------- viewer ----------

var errNoNode = errors.New("the viewer could not start; see the log")

// ViewerState is this app's own tailnet login state.
func (a *App) ViewerState() (tsnode.State, error) {
	if a.node == nil {
		return tsnode.State{}, errNoNode
	}
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
	defer cancel()
	return a.node.State(ctx)
}

// SignIn opens the tailnet login page.
func (a *App) SignIn() error {
	st, err := a.ViewerState()
	if err != nil {
		return err
	}
	if st.AuthURL == "" {
		return errors.New("no login link yet; try again in a moment")
	}
	return a.OpenURL(st.AuthURL)
}

// Bridge is how the page reaches the session websocket.
type Bridge struct {
	URL      string `json:"url"`
	Protocol string `json:"protocol"`
}

func (a *App) Bridge() (Bridge, error) {
	if a.bridge == nil {
		return Bridge{}, errNoNode
	}
	return Bridge{URL: a.bridge.WSURL(), Protocol: a.bridge.Subprotocol()}, nil
}

// ListHosts lists hosts on the tailnet.
func (a *App) ListHosts() ([]tsnode.Peer, error) {
	if a.node == nil {
		return nil, errNoNode
	}
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
	defer cancel()
	p, err := a.node.Hosts(ctx, config.HostTag)
	if p == nil {
		p = []tsnode.Peer{}
	}
	return p, err
}

// ---------- files (in-process via the bridge) ----------

func (a *App) ListFiles(sid string) (proto.FileList, error) {
	if a.bridge == nil {
		return proto.FileList{}, errNoNode
	}
	return a.bridge.ListFiles(sid)
}

// Transfer is a progress event ("transfer") for the files panel.
type Transfer struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Dir   string `json:"dir"` // "up" | "down"
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
	State string `json:"state"` // "active" | "ok" | "error"
	Error string `json:"error,omitempty"`
	Path  string `json:"path,omitempty"` // local path of a finished download
}

func (a *App) emit(t Transfer) { wruntime.EventsEmit(a.ctx, "transfer", t) }

var transferSeq struct {
	sync.Mutex
	n int
}

func newTransferID() string {
	transferSeq.Lock()
	defer transferSeq.Unlock()
	transferSeq.n++
	return fmt.Sprintf("t%d", transferSeq.n)
}

// progress counts bytes and emits at most every 150 ms.
type progress struct {
	a    *App
	t    Transfer
	last time.Time
}

func (p *progress) add(n int) {
	p.t.Done += int64(n)
	if time.Since(p.last) > 150*time.Millisecond {
		p.last = time.Now()
		p.a.emit(p.t)
	}
}

type countReader struct {
	r io.Reader
	p *progress
}

func (c countReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.p.add(n)
	return n, err
}

type countWriter struct {
	w io.Writer
	p *progress
}

func (c countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.p.add(n)
	return n, err
}

// UploadDialog lets the user pick files and sends them to the host.
func (a *App) UploadDialog(sid string) error {
	paths, err := wruntime.OpenMultipleFilesDialog(a.ctx, wruntime.OpenDialogOptions{Title: "Send files to the remote computer"})
	if err != nil {
		return err
	}
	a.UploadPaths(sid, paths)
	return nil
}

// UploadPaths sends local files (e.g. dropped on the window) to the host.
// Progress is reported through "transfer" events.
func (a *App) UploadPaths(sid string, paths []string) {
	for _, p := range paths {
		go a.uploadOne(sid, p)
	}
}

func (a *App) uploadOne(sid, path string) {
	pr := &progress{a: a, t: Transfer{ID: newTransferID(), Name: filepath.Base(path), Dir: "up", State: "active"}}
	fail := func(err error) { pr.t.State, pr.t.Error = "error", err.Error(); a.emit(pr.t) }
	f, err := os.Open(path)
	if err != nil {
		fail(err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		fail(err)
		return
	}
	if !fi.Mode().IsRegular() {
		fail(errors.New("only files can be sent (not folders)"))
		return
	}
	pr.t.Total = fi.Size()
	a.emit(pr.t)
	done, err := a.bridge.UploadFile(a.ctx, sid, pr.t.Name, fi.Size(), countReader{f, pr})
	if err != nil {
		fail(err)
		return
	}
	pr.t.Name, pr.t.Done, pr.t.State = done.Name, done.Size, "ok"
	a.emit(pr.t)
}

// Download asks where to save a host file, then streams it there. The file
// only appears under its final name once its checksum has verified.
func (a *App) Download(sid, name string) error {
	dest, err := wruntime.SaveFileDialog(a.ctx, wruntime.SaveDialogOptions{Title: "Save file from the remote computer", DefaultFilename: name})
	if err != nil || dest == "" {
		return err
	}
	go a.downloadTo(sid, name, dest)
	return nil
}

func (a *App) downloadTo(sid, name, dest string) {
	pr := &progress{a: a, t: Transfer{ID: newTransferID(), Name: name, Dir: "down", State: "active"}}
	fail := func(err error) { pr.t.State, pr.t.Error = "error", err.Error(); a.emit(pr.t) }
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".rmt-download-*")
	if err != nil {
		fail(err)
		return
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	err = a.bridge.DownloadFile(a.ctx, sid, name, func(ack proto.FileAck) {
		pr.t.Total = ack.Size
		a.emit(pr.t)
	}, countWriter{tmp, pr})
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dest)
	}
	if err != nil {
		fail(err)
		return
	}
	pr.t.State, pr.t.Path = "ok", dest
	a.emit(pr.t)
}

// ---------- this device (host) ----------

// HostView is the "This device" card.
type HostView struct {
	Service string          `json:"service"` // hostsetup.NotInstalled | Running | Stopped
	Status  *hostctl.Status `json:"status,omitempty"`
	Error   string          `json:"error,omitempty"`
}

func (a *App) HostStatus() HostView {
	exe, _ := hostsetup.RmtdPath()
	v := HostView{Service: hostsetup.ServiceState(a.hostDir, exe)}
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Second)
	defer cancel()
	st, err := hostctl.GetStatus(ctx, a.hostDir)
	if err == nil {
		v.Status = &st
		if v.Service == hostsetup.NotInstalled {
			v.Service = hostsetup.Running // e.g. `rmtd up` in a terminal
		}
	} else if v.Service == hostsetup.Running {
		v.Error = "starting…"
	}
	return v
}

// EnableHost starts host setup. On Windows it relaunches the app elevated
// (UAC) in setup mode and returns "elevated"; elsewhere setup runs in this
// window and it returns "inprocess".
func (a *App) EnableHost() (string, error) {
	if !a.Info().HostSupported {
		return "", errors.New(a.Info().HostNote)
	}
	if runtime.GOOS != "windows" {
		return "inprocess", nil
	}
	args := []string{"--host-setup"}
	if st, err := a.ViewerState(); err == nil && st.LoginName != "" {
		args = append(args, "--allow="+st.LoginName)
	}
	if err := relaunchElevated(args...); err != nil {
		return "", err
	}
	return "elevated", nil
}

// DisableHost stops and removes the host service (elevated on Windows).
func (a *App) DisableHost() error {
	if runtime.GOOS == "windows" {
		return relaunchElevated("--host-disable")
	}
	exe, err := hostsetup.RmtdPath()
	if err != nil {
		return err
	}
	return hostsetup.Disable(a.hostDir, exe)
}

// DisconnectSessions ends every session controlling this computer.
func (a *App) DisconnectSessions() (int, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
	defer cancel()
	return hostctl.Disconnect(ctx, a.hostDir)
}

// ---------- host setup wizard ----------

// SetupInfo is the wizard's starting point.
type SetupInfo struct {
	HostName    string   `json:"hostName"`
	Allowed     []string `json:"allowed"`
	Prefill     string   `json:"prefill"`
	Mode        string   `json:"mode"`
	PasswordSet bool     `json:"passwordSet"`
	TOTPSet     bool     `json:"totpSet"`
	LoggedIn    bool     `json:"loggedIn"`
	Service     string   `json:"service"`
	Codec       bool     `json:"codec"`
	PolicyHelp  string   `json:"policyHelp"`
}

func (a *App) SetupInfo() (SetupInfo, error) {
	c, err := hostsetup.Load(a.hostDir)
	if err != nil {
		return SetupInfo{}, err
	}
	exe, _ := hostsetup.RmtdPath()
	in := SetupInfo{
		HostName: c.Hostname, Allowed: append(c.Policy.AllowedLogins, c.Policy.AllowedTags...),
		Prefill: a.prefill, Mode: string(c.Access.Mode),
		PasswordSet: c.Access.PasswordHash != "", TOTPSet: c.Access.TOTPSecret != "",
		LoggedIn: hostsetup.LoggedIn(a.hostDir), Service: hostsetup.ServiceState(a.hostDir, exe),
		Codec: hostsetup.CodecInstalled(a.hostDir), PolicyHelp: tsnode.TagPolicySnippet,
	}
	if in.Prefill == "" && !a.setupMode {
		if st, err := a.ViewerState(); err == nil {
			in.Prefill = st.LoginName
		}
	}
	return in, nil
}

// SetupRequest saves who may connect and how they are admitted.
type SetupRequest struct {
	Allowed       []string `json:"allowed"`
	Mode          string   `json:"mode"`
	Password      string   `json:"password"` // "" keeps the current one
	ClearPassword bool     `json:"clearPassword"`
}

func (a *App) SetupSave(r SetupRequest) error {
	return hostsetup.Edit(a.hostDir, func(c *config.Host) error {
		var nc config.Host = *c
		nc.Policy.AllowedLogins, nc.Policy.AllowedTags = nil, nil
		for _, who := range r.Allowed {
			if strings.TrimSpace(who) == "" {
				continue
			}
			if err := hostsetup.Allow(&nc, who); err != nil {
				return err
			}
		}
		if len(nc.Policy.AllowedLogins)+len(nc.Policy.AllowedTags) == 0 {
			return errors.New("add at least one person (tailnet login) who may connect")
		}
		if r.ClearPassword {
			hostsetup.ClearPassword(&nc)
		} else if r.Password != "" {
			if err := hostsetup.SetPassword(&nc, r.Password); err != nil {
				return err
			}
		}
		if err := hostsetup.SetMode(&nc, accessMode(r.Mode)); err != nil {
			return err
		}
		*c = nc
		return nil
	})
}

// TOTPEnroll is shown once: secret, otpauth URI and a QR code of it.
type TOTPEnroll struct {
	Secret string `json:"secret"`
	URI    string `json:"uri"`
	QR     string `json:"qr"` // data: URL (PNG)
}

func (a *App) SetupTOTPBegin() (TOTPEnroll, error) {
	c, err := hostsetup.Load(a.hostDir)
	if err != nil {
		return TOTPEnroll{}, err
	}
	sec, uri, err := hostsetup.NewTOTP(&c)
	if err != nil {
		return TOTPEnroll{}, err
	}
	code, err := qr.Encode(uri, qr.M)
	if err != nil {
		return TOTPEnroll{}, err
	}
	code.Scale = 6
	return TOTPEnroll{Secret: sec, URI: uri, QR: "data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG())}, nil
}

func (a *App) SetupTOTPConfirm(secret, code string) error {
	return hostsetup.Edit(a.hostDir, func(c *config.Host) error { return hostsetup.EnableTOTP(c, secret, code) })
}

func (a *App) SetupTOTPDisable() error {
	return hostsetup.Edit(a.hostDir, func(c *config.Host) error { c.Access.TOTPSecret = ""; return nil })
}

// LoginEvent ("setup:login") reports host sign-in progress.
type LoginEvent struct {
	State     tsnode.State `json:"state"`
	Done      bool         `json:"done"`
	Error     string       `json:"error,omitempty"`
	TagDenied bool         `json:"tagDenied,omitempty"`
}

// SetupLogin signs this computer in as a host (tag:rmt-host). Progress
// arrives as "setup:login" events; open State.AuthURL with OpenURL.
func (a *App) SetupLogin() error {
	exe, _ := hostsetup.RmtdPath()
	if hostsetup.ServiceState(a.hostDir, exe) == hostsetup.Running {
		// The node's state can only be used by one process at a time.
		if err := hostsetup.Control(a.hostDir, exe, "stop"); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	a.loginMu.Lock()
	if a.loginCancel != nil {
		a.loginCancel()
	}
	a.loginCancel = cancel
	a.loginMu.Unlock()
	go func() {
		defer cancel()
		err := hostsetup.Login(ctx, a.hostDir, func(st tsnode.State) {
			wruntime.EventsEmit(a.ctx, "setup:login", LoginEvent{State: st})
		})
		ev := LoginEvent{Done: err == nil}
		if err != nil && ctx.Err() == nil {
			ev.Error = err.Error()
			var te *hostsetup.TagError
			ev.TagDenied = errors.As(err, &te)
			if te != nil {
				ev.Error = te.Msg
			}
		}
		if err == nil || ctx.Err() == nil {
			wruntime.EventsEmit(a.ctx, "setup:login", ev)
		}
	}()
	return nil
}

// SetupCodec downloads the H.264 library (Cisco OpenH264).
func (a *App) SetupCodec() error {
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Minute)
	defer cancel()
	_, err := hostsetup.InstallCodec(ctx, a.hostDir)
	return err
}

// SetupFinish installs (if needed) and (re)starts the host service.
func (a *App) SetupFinish() error {
	exe, err := hostsetup.RmtdPath()
	if err != nil {
		return err
	}
	return hostsetup.Enable(a.hostDir, exe)
}

// CloseWindow ends the setup window (setup mode only).
func (a *App) CloseWindow() {
	if a.setupMode {
		wruntime.Quit(a.ctx)
	}
}

func accessMode(s string) access.Mode { return access.Mode(s) }

// Confirm asks a yes/no question in a native dialog.
func (a *App) Confirm(title, message string) (bool, error) {
	r, err := wruntime.MessageDialog(a.ctx, wruntime.MessageDialogOptions{
		Type: wruntime.QuestionDialog, Title: title, Message: message,
		Buttons: []string{"Yes", "No"}, DefaultButton: "No", CancelButton: "No",
	})
	if err != nil {
		return false, err
	}
	return r == "Yes" || r == "Ok", nil
}

// OpenPrivacySettings opens the macOS Privacy pane rmtd needs
// ("screen" or "accessibility").
func (a *App) OpenPrivacySettings(pane string) error { return openPrivacyPane(pane) }
