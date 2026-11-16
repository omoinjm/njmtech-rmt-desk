// Platform adapter: the same UI runs inside the rmt-desk app (Wails:
// native bindings at window.go.main.App) or in a browser tab opened by
// `rmt ui` (same-origin HTTP API + cookie).
"use strict";

const G = window.go?.main?.App;
export const isApp = !!G;

// Tiny event hub for browser-mode transfer progress.
const listeners = new Set();
function emitTransfer(t) { for (const cb of listeners) cb(t); }

let transferSeq = 0;

async function httpJSON(url) {
  const res = await fetch(url);
  if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
  return res.json();
}

const browser = {
  info: async () => ({ mode: "main", os: "", hostSupported: false, browser: true }),
  viewerState: async () => ({ Running: true }),
  signIn: async () => {},
  listHosts: () => httpJSON("/api/hosts"),
  openSocket: async (host) => new WebSocket(`ws://${location.host}/ws?host=${encodeURIComponent(host)}`),
  listFiles: (sid) => httpJSON(`/api/files?sid=${encodeURIComponent(sid)}`),
  pickAndUpload: (sid) => {
    const inp = document.getElementById("upload");
    inp.onchange = () => { for (const f of inp.files) uploadBlob(sid, f); inp.value = ""; };
    inp.click();
  },
  uploadFiles: (sid, files) => { for (const f of files) uploadBlob(sid, f); },
  download: async (sid, name) => {
    const a = document.createElement("a");
    a.href = `/api/download?sid=${encodeURIComponent(sid)}&name=${encodeURIComponent(name)}`;
    a.download = name;
    document.body.append(a);
    a.click();
    a.remove();
  },
  onTransfer: (cb) => listeners.add(cb),
  onFileDrop: null, // HTML5 drag & drop is used instead
  toggleFullscreen: async () => {
    if (document.fullscreenElement) await document.exitFullscreen();
    else await document.documentElement.requestFullscreen();
  },
  openURL: async (u) => { window.open(u, "_blank", "noopener"); },
  confirm: async (title, msg) => window.confirm(title + "\n\n" + msg),
  copyText: (s) => navigator.clipboard.writeText(s),
};

function uploadBlob(sid, file) {
  const t = { id: "b" + (++transferSeq), name: file.name, dir: "up", done: 0, total: file.size, state: "active" };
  emitTransfer({ ...t });
  const xhr = new XMLHttpRequest();
  xhr.open("POST", `/api/upload?sid=${encodeURIComponent(sid)}&name=${encodeURIComponent(file.name)}`);
  xhr.setRequestHeader("X-Rmt-Upload", "1");
  xhr.upload.onprogress = (e) => { t.done = e.loaded; emitTransfer({ ...t }); };
  xhr.onload = () => {
    if (xhr.status === 200) {
      const d = JSON.parse(xhr.responseText);
      emitTransfer({ ...t, name: d.name, done: d.size, state: "ok" });
    } else {
      emitTransfer({ ...t, state: "error", error: xhr.responseText.trim() || `failed (${xhr.status})` });
    }
  };
  xhr.onerror = () => emitTransfer({ ...t, state: "error", error: "connection failed" });
  xhr.send(file);
}

const app = isApp && {
  info: () => G.Info(),
  viewerState: () => G.ViewerState(),
  signIn: () => G.SignIn(),
  listHosts: () => G.ListHosts(),
  openSocket: async (host) => {
    const b = await G.Bridge();
    return new WebSocket(`${b.url}?host=${encodeURIComponent(host)}`, [b.protocol]);
  },
  listFiles: (sid) => G.ListFiles(sid),
  pickAndUpload: (sid) => G.UploadDialog(sid),
  uploadFiles: null, // the web view can't give us paths; drops arrive via onFileDrop
  download: (sid, name) => G.Download(sid, name),
  onTransfer: (cb) => window.runtime.EventsOn("transfer", cb),
  onFileDrop: (cb) => window.runtime.OnFileDrop((_x, _y, paths) => cb(paths), false),
  uploadPaths: (sid, paths) => G.UploadPaths(sid, paths),
  toggleFullscreen: async () => {
    if (await window.runtime.WindowIsFullscreen()) window.runtime.WindowUnfullscreen();
    else window.runtime.WindowFullscreen();
  },
  openURL: (u) => G.OpenURL(u),
  copyText: (s) => G.CopyText(s),
  confirm: (title, msg) => G.Confirm(title, msg),
  on: (ev, cb) => window.runtime.EventsOn(ev, cb),
  // This device / host setup
  hostStatus: () => G.HostStatus(),
  enableHost: () => G.EnableHost(),
  disableHost: () => G.DisableHost(),
  disconnectSessions: () => G.DisconnectSessions(),
  setupInfo: () => G.SetupInfo(),
  setupSave: (r) => G.SetupSave(r),
  setupTOTPBegin: () => G.SetupTOTPBegin(),
  setupTOTPConfirm: (s, c) => G.SetupTOTPConfirm(s, c),
  setupTOTPDisable: () => G.SetupTOTPDisable(),
  setupLogin: () => G.SetupLogin(),
  setupCodec: () => G.SetupCodec(),
  setupFinish: () => G.SetupFinish(),
  openPrivacySettings: (pane) => G.OpenPrivacySettings(pane),
  closeWindow: () => G.CloseWindow(),
};

export const api = isApp ? app : browser;
