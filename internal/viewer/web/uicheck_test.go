//go:build uicheck

package web

import (
	"bytes"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// go test -tags uicheck -run TestUIServe ./internal/viewer/web
// Serves the UI with a mock of the app's native bindings (window.go) for
// headless-browser checks. The session view uses the real bridge (embedded
// mode) and a fake host.
func TestUIServe(t *testing.T) {
	e := setupEnv(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	origin := "http://" + ln.Addr().String()
	e.srv.EmbedOrigins = append(e.srv.EmbedOrigins, origin)
	assets := Assets()
	mock := strings.NewReplacer("__WS__", e.srv.WSURL(), "__PROTO__", e.srv.Subprotocol()).Replace(mockJS)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := fs.ReadFile(assets, "index.html")
		b = bytes.Replace(b, []byte(`<script type="module" src="app.js">`), []byte(`<script src="mock.js"></script><script type="module" src="app.js">`), 1)
		if d := r.URL.Query().Get("hold"); d != "" {
			// Delays the load event (when headless Chrome screenshots) in
			// real time, so the session can actually run first.
			b = bytes.Replace(b, []byte(`</body>`), []byte(`<img src="/slow?d=`+d+`" alt="" hidden></body>`), 1)
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write(b)
	})
	mux.HandleFunc("GET /mock.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		w.Write([]byte(mock))
	})
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		// Headless Chrome's virtual time stops while a fetch is pending:
		// this holds the clock so real network work (the session) happens.
		d, _ := time.ParseDuration(r.URL.Query().Get("d"))
		time.Sleep(d)
	})
	mux.Handle("GET /", http.FileServerFS(assets))
	go http.Serve(ln, mux)
	os.WriteFile(os.Getenv("UICHECK_URL_FILE"), []byte(origin+"/"), 0o600)
	fmt.Println("UI:", origin)
	time.Sleep(240 * time.Second)
}

const mockJS = `
const q = new URLSearchParams(location.search);
const s = q.get("s") || "home";
const later = (f, ms) => setTimeout(f, ms);
const ev = {};
window.runtime = {
  EventsOn: (n, cb) => { (ev[n] = ev[n] || []).push(cb); },
  OnFileDrop: () => {}, WindowIsFullscreen: async () => false, WindowFullscreen() {}, WindowUnfullscreen() {},
};
const emit = (n, d) => (ev[n] || []).forEach((cb) => cb(d));
const ok = (v) => Promise.resolve(v);
window.go = { main: { App: {
  Info: () => ok({ mode: s.startsWith("setup") ? "setup" : "main", os: "windows", version: "dev", hostSupported: true }),
  ViewerState: () => ok(s === "signin" ? { Running: false, AuthURL: "https://login.tailscale.com/a/x" } : { Running: true, LoginName: "you@example.com" }),
  SignIn: () => ok(), OpenURL: () => ok(), CopyText: () => ok(), Confirm: () => ok(true),
  ListHosts: () => ok([
    { HostName: "rmt-box", OS: "windows", IP: "100.64.0.2", Online: true },
    { HostName: "rmt-studio-mac", OS: "macOS", IP: "100.64.0.7", Online: true },
    { HostName: "rmt-old-laptop", OS: "linux", IP: "100.64.0.9", Online: false }]),
  Bridge: () => ok({ url: "__WS__", protocol: "__PROTO__" }),
  ListFiles: () => ok({ dir: "C:\\Users\\you\\Downloads\\rmt-desk", files: [{ name: "report.pdf", size: 1843200 }, { name: "photo.jpg", size: 524288 }] }),
  UploadDialog: () => { emit("transfer", { id: "t1", name: "notes.txt", dir: "up", done: 300000, total: 1000000, state: "active" }); return ok(); },
  UploadPaths: () => ok(), Download: () => ok(),
  HostStatus: () => ok(s === "home-off" ? { service: "not installed" } : { service: "running", status: {
    hostName: "rmt-desktop-9kjg7sg", tailnetName: "rmt-desktop-9kjg7sg.tail1234.ts.net", ip: "100.99.23.92", online: true,
    mode: "both", passwordSet: true, totpSet: false, allowed: ["you@example.com"],
    sessions: s === "home-controlled" ? [{ peer: "rmt-viewer-laptop (you@example.com)", since: new Date().toISOString(), granted: ["view"] }] : [] } }),
  EnableHost: () => ok("elevated"), DisableHost: () => ok(), DisconnectSessions: () => ok(1),
  SetupInfo: () => ok({ hostName: "rmt-desktop-9kjg7sg", allowed: [], prefill: "you@example.com", mode: "both", passwordSet: s === "setup2b",
    totpSet: false, loggedIn: false, service: "not installed", codec: false, policyHelp: '"tagOwners": {\n  "tag:rmt-host": ["autogroup:admin"],\n},' }),
  SetupSave: () => ok(), SetupTOTPBegin: () => ok({ secret: "JBSWY3DPEHPK3PXP", uri: "otpauth://x", qr: "" }), SetupTOTPConfirm: () => ok(),
  SetupTOTPDisable: () => ok(), SetupCodec: () => ok(), SetupFinish: () => ok(), CloseWindow: () => ok(), OpenPrivacySettings: () => ok(),
  SetupLogin: () => { later(() => emit("setup:login", { state: { AuthURL: "https://login.tailscale.com/a/y" } }), 100);
    if (s === "setup3tag") later(() => emit("setup:login", { error: "requested tags [tag:rmt-host] are invalid or not permitted", tagDenied: true }), 300);
    return ok(); },
} } };
const RealWS = window.WebSocket;
const t0 = performance.now();
window.WebSocket = function (url, protos) {
  const w = new RealWS(url, protos);
  const log = (m) => console.log("WS " + Math.round(performance.now() - t0) + "ms " + m);
  log("new " + url.split("?")[0]);
  w.addEventListener("open", () => log("open proto=" + w.protocol));
  w.addEventListener("message", (e) => log("msg " + (typeof e.data === "string" ? e.data.slice(0, 80) : "binary " + e.data.byteLength)));
  w.addEventListener("close", (e) => log("close " + e.code));
  w.addEventListener("error", () => log("error"));
  return w;
};
window.WebSocket.OPEN = 1;
const click = (sel) => document.querySelector(sel)?.click();
document.addEventListener("DOMContentLoaded", () => {
  if (s === "session" || s === "session-files") later(() => click("#hosts li"), 800);
  if (s === "session-files") later(() => { click("#files-btn"); click("#stats-btn"); click("#upload-btn"); }, 2500);
  if (s === "setup2" || s === "setup2b" || s.startsWith("setup3")) later(() => click("#wiz-next"), 500);
  if (s.startsWith("setup3")) later(() => { document.getElementById("pw1").value = document.getElementById("pw2").value = "correct horse battery"; click("#wiz-next"); }, 900);
  if (s.startsWith("setup3")) later(() => document.querySelector("#login-body button")?.click(), 1500);
});
`
