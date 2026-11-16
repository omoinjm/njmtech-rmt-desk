// Remote session view: video (JPEG tiles or H.264 via WebCodecs), input,
// clipboard toggle, files drawer, stats, reconnect.
"use strict";

import { api } from "./platform.js";
import { $, el, icon, show, toast, errText, fmtSize, refusalText, store } from "./ui.js";

let onLeave = () => {};
export function setOnLeave(f) { onLeave = f; }

let ws = null;
let display = 0;
let viewOnly = false;
let currentHost = null;
let creds = { password: "", totp: "" }; // memory only, per host
let sid = null;
let granted = [];
let connectedOnce = false;  // this attempt reached "hello"
let wantOpen = false;       // user hasn't left the session
let autoRetried = false;
let quietClose = false;     // the host asked for a password: the socket's close is expected
const canvas = $("screen");
const ctx2d = canvas.getContext("2d");
const held = new Set();

function status(s) { $("status").textContent = s || ""; }
function dot(state) { $("conn-dot").className = "dot " + (state || ""); }

function overlay(msg, { spin = true, retry = false } = {}) {
  const o = $("overlay");
  if (msg == null) { o.hidden = true; return; }
  o.hidden = false;
  o.classList.toggle("idle", !spin);
  $("overlay-msg").textContent = msg;
  $("overlay-retry").hidden = !retry;
}

function send(type, data) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type, data }));
}

// ---------- codec negotiation ----------
const H264_CODEC = "avc1.42E033"; // Constrained Baseline, level 5.1
let h264Supported = false;
let currentCodec = "tiles";
(async () => {
  try {
    if ("VideoDecoder" in window) {
      const r = await VideoDecoder.isConfigSupported({ codec: H264_CODEC, optimizeForLatency: true });
      h264Supported = !!r.supported;
    }
  } catch { h264Supported = false; }
  if (!h264Supported) $("codec").title = "This system can't decode H.264 here; JPEG tiles are used.";
})();

function codecs() {
  return h264Supported && $("codec").value === "auto" ? ["h264", "tiles"] : ["tiles"];
}
function videoReq() {
  send("video", { display, fps: +$("fps").value, quality: +$("quality").value, codecs: codecs() });
}

// ---------- connect ----------
export async function connect(host) {
  if (host !== currentHost) creds = { password: "", totp: "" };
  currentHost = host;
  wantOpen = true;
  $("auth").hidden = true;
  show("session");
  $("title").textContent = host;
  status("");
  dot("warn");
  overlay(`Connecting to ${host}…`);
  viewOnly = false;
  connectedOnce = false;
  quietClose = false;
  if (ws) { const old = ws; ws = null; old.close(); }

  let sock;
  try {
    sock = await api.openSocket(host);
  } catch (e) {
    overlay("Could not start the connection: " + errText(e), { spin: false, retry: true });
    return;
  }
  ws = sock;
  sock.binaryType = "arraybuffer";
  sock.onopen = () => {
    sock.send(JSON.stringify({ type: "connect", data: creds }));
    creds.totp = ""; // one-time code: never reuse
  };
  sock.onmessage = (ev) => {
    if (ws !== sock) return; // stale socket from a previous attempt
    if (typeof ev.data !== "string") {
      stats.bytes += ev.data.byteLength;
      const tag = new Uint8Array(ev.data, 0, 1)[0]; // 0 = JPEG tiles, 1 = H.264
      if (tag === 0) enqueueFrame(ev.data.slice(1));
      else if (tag === 1) h264Frame(ev.data);
      return;
    }
    onMessage(JSON.parse(ev.data));
  };
  sock.onclose = () => {
    if (ws !== sock) return;
    ws = null;
    dot("");
    if (quietClose) { quietClose = false; return; }
    if (!wantOpen) return;
    if (connectedOnce && !autoRetried) {
      autoRetried = true;
      overlay("Connection lost. Reconnecting…");
      setTimeout(() => { if (wantOpen && !ws) connect(currentHost); }, 1500);
      return;
    }
    if (connectedOnce) {
      overlay("Disconnected.", { spin: false, retry: true });
    } else if ($("overlay").hidden || $("overlay-retry").hidden) {
      // Never got in, and no refusal message is showing: say so.
      overlay(`Could not connect to ${currentHost}.`, { spin: false, retry: true });
    }
  };
}

function onMessage(m) {
  switch (m.type) {
    case "hello":
      connectedOnce = true;
      autoRetried = false;
      sid = m.data.sid;
      granted = m.data.granted || [];
      $("files-btn").hidden = !granted.includes("files");
      $("clip").hidden = !granted.includes("clipboard");
      if (!$("clip").classList.contains("on")) send("clipboard", { enabled: false });
      status(`${{ windows: "Windows", darwin: "macOS", linux: "Linux" }[m.data.os] || m.data.os} · you are ${m.data.identity}`);
      dot("on");
      overlay(null);
      rememberRecent(currentHost);
      videoReq();
      canvas.focus();
      break;
    case "info": {
      const sel = $("display");
      sel.textContent = "";
      for (const d of m.data.displays) {
        sel.append(el("option", { value: d.index }, `${d.index + 1}: ${d.width}×${d.height}`));
      }
      sel.parentElement.hidden = m.data.displays.length < 2;
      display = m.data.current;
      sel.value = display;
      if (m.data.codec !== currentCodec || m.data.codec === "h264") {
        currentCodec = m.data.codec;
        if (currentCodec === "h264") setupDecoder(); else closeDecoder();
      }
      break;
    }
    case "status":
      overlay(`Connecting to ${currentHost}… If that computer asks for approval, someone there needs to click Allow.`);
      break;
    case "clipboard":
      if (m.error) toast(m.error, "error");
      break;
    case "view-only":
      viewOnly = true;
      status("view only: " + m.error);
      toast("View only: " + m.error);
      break;
    case "error":
      if (m.code === "auth_required" || m.code === "auth_failed") {
        overlay(null);
        quietClose = true;
        showAuth(m.code === "auth_failed" ? refusalText(m.code) : "This computer requires a password.");
        return;
      }
      if (!connectedOnce) {
        overlay(refusalText(m.code, m.error), { spin: false, retry: true });
        autoRetried = true; // a refusal isn't a dropped connection
      } else if (m.error !== "session closed") {
        toast(m.error, "error");
      }
      break;
  }
}

// ---------- password prompt ----------
function showAuth(message) {
  $("auth-msg").textContent = message;
  $("auth-pw").value = "";
  $("auth-totp").value = "";
  $("auth").hidden = false;
  $("auth-pw").focus();
}
$("auth").onsubmit = (e) => {
  e.preventDefault();
  creds = { password: $("auth-pw").value, totp: $("auth-totp").value.trim() };
  connect(currentHost);
};
$("auth-ask").onclick = () => { creds = { password: "", totp: "" }; connect(currentHost); };

$("overlay-retry").onclick = () => { autoRetried = false; connect(currentHost); };
$("overlay-cancel").onclick = () => leave();

// ---------- leaving ----------
export function leave() {
  wantOpen = false;
  closeDecoder();
  currentCodec = "tiles";
  sid = null;
  $("files").hidden = true;
  $("files-btn").classList.remove("on");
  $("transfers").textContent = "";
  transfers.clear();
  creds = { password: "", totp: "" };
  $("auth").hidden = true;
  overlay(null);
  releaseAll();
  if (ws) { const s = ws; ws = null; s.close(); }
  ctx2d.clearRect(0, 0, canvas.width, canvas.height);
  onLeave();
}
$("disconnect").onclick = leave;

// ---------- toolbar ----------
$("display").onchange = (e) => { display = +e.target.value; videoReq(); };
$("quality").onchange = videoReq;
$("fps").onchange = videoReq;
$("codec").onchange = videoReq;
$("fit").onclick = (e) => {
  const on = $("stage").classList.toggle("fit");
  e.currentTarget.classList.toggle("on", on);
};
$("clip").onclick = (e) => {
  const on = e.currentTarget.classList.toggle("on");
  send("clipboard", { enabled: on });
  toast(on ? "Clipboard sync on" : "Clipboard sync off");
};
$("full").onclick = () => api.toggleFullscreen().catch(() => {});
$("stats-btn").onclick = (e) => {
  const on = e.currentTarget.classList.toggle("on");
  $("stats").hidden = !on;
};

// ---------- stats ----------
const stats = { frames: 0, bytes: 0 };
setInterval(() => {
  if (!$("stats").hidden) {
    $("stats").textContent =
      `${canvas.width}×${canvas.height}  ${currentCodec === "h264" ? "H.264" : "JPEG tiles"}\n` +
      `${stats.frames} fps  ${(stats.bytes * 8 / 1e6).toFixed(2)} Mbit/s`;
  }
  stats.frames = 0;
  stats.bytes = 0;
}, 1000);

// ---------- recent ----------
function rememberRecent(host) {
  const r = store.get("rmt.recent", []).filter((h) => h !== host);
  r.unshift(host);
  store.set("rmt.recent", r.slice(0, 6));
}

// ---------- files ----------
const transfers = new Map(); // id -> li

async function loadFiles() {
  const ul = $("file-list");
  if (!sid) return;
  ul.textContent = "";
  ul.append(el("li", { class: "empty" }, "Loading…"));
  try {
    const l = await api.listFiles(sid);
    $("files-dir").textContent = l.dir;
    ul.textContent = "";
    if (!l.files || !l.files.length) {
      ul.append(el("li", { class: "empty" }, "Empty. Files you send appear here."));
      return;
    }
    for (const f of l.files) {
      const dl = el("button", { class: "icon", type: "button", title: "Save to this computer",
        onclick: () => api.download(sid, f.name).catch((e) => toast(errText(e), "error")) }, icon("download"));
      ul.append(el("li", {}, el("span", { class: "name" }, f.name), dl, el("span", { class: "sub" }, fmtSize(f.size))));
    }
  } catch (e) {
    ul.textContent = "";
    ul.append(el("li", { class: "empty" }, "Error: " + errText(e)));
  }
}

api.onTransfer((t) => {
  let li = transfers.get(t.id);
  if (!li) {
    li = el("li", {}, el("span", { class: "name" }), el("span", { class: "sub" }), el("div", { class: "bar" }, el("i")));
    transfers.set(t.id, li);
    $("transfers").prepend(li);
    $("files").hidden = false;
    $("files-btn").classList.add("on");
  }
  const [name, sub, bar] = li.children;
  name.textContent = (t.dir === "up" ? "↑ " : "↓ ") + t.name;
  const pct = t.total ? Math.min(100, (t.done / t.total) * 100) : 0;
  if (bar && bar.firstChild) bar.firstChild.style.width = pct + "%";
  if (t.state === "active") {
    sub.textContent = `${fmtSize(t.done)} / ${fmtSize(t.total || 0)}`;
  } else if (t.state === "ok") {
    sub.textContent = "✓ verified";
    bar?.remove();
    if (t.dir === "up") loadFiles();
  } else {
    sub.textContent = "failed";
    bar?.remove();
    li.append(el("span", { class: "err" }, t.error || "failed"));
  }
});

$("files-btn").onclick = (e) => {
  const p = $("files");
  p.hidden = !p.hidden;
  e.currentTarget.classList.toggle("on", !p.hidden);
  if (!p.hidden) loadFiles();
};
$("files-close").onclick = () => { $("files").hidden = true; $("files-btn").classList.remove("on"); };
$("files-refresh").onclick = loadFiles;
$("upload-btn").onclick = () => {
  if (sid) Promise.resolve(api.pickAndUpload(sid)).catch((e) => toast(errText(e), "error"));
};

function canDrop() { return sid && granted.includes("files") && !$("session").hidden; }

if (api.onFileDrop) {
  // App: the OS delivers dropped files as paths.
  api.onFileDrop((paths) => { if (canDrop() && paths?.length) api.uploadPaths(sid, paths); });
  const stage = $("stage");
  stage.addEventListener("dragover", (e) => { if (canDrop()) { e.preventDefault(); $("drop").hidden = false; } });
  stage.addEventListener("dragleave", () => { $("drop").hidden = true; });
  stage.addEventListener("drop", () => { $("drop").hidden = true; });
} else {
  const stage = $("stage");
  let depth = 0;
  stage.addEventListener("dragenter", (e) => {
    if (!canDrop() || !e.dataTransfer.types.includes("Files")) return;
    e.preventDefault(); depth++; $("drop").hidden = false;
  });
  stage.addEventListener("dragover", (e) => { if (!$("drop").hidden) e.preventDefault(); });
  stage.addEventListener("dragleave", () => { if (--depth <= 0) { depth = 0; $("drop").hidden = true; } });
  stage.addEventListener("drop", (e) => {
    if ($("drop").hidden) return;
    e.preventDefault(); depth = 0; $("drop").hidden = true;
    api.uploadFiles(sid, e.dataTransfer.files);
  });
}

// ---------- video: JPEG tiles ----------
// Frame: u16 w | u16 h | u16 n | n × (u16 x | u16 y | u16 w | u16 h | u32 len | jpeg)
let chain = Promise.resolve();
let pending = 0;
let needKeyframe = false;
function enqueueFrame(buf) {
  // Drop frames if decoding falls behind (bounded latency); a dropped delta
  // frame could leave stale tiles, so ask for a full frame afterwards.
  if (pending > 3) { needKeyframe = true; return; }
  pending++;
  chain = chain.then(() => drawFrame(buf)).catch(() => {}).finally(() => {
    pending--;
    if (needKeyframe && pending === 0) { needKeyframe = false; videoReq(); }
  });
}

async function drawFrame(buf) {
  const v = new DataView(buf);
  const w = v.getUint16(0), h = v.getUint16(2), n = v.getUint16(4);
  if (w !== canvas.width || h !== canvas.height) { canvas.width = w; canvas.height = h; }
  let off = 6;
  const jobs = [];
  for (let i = 0; i < n; i++) {
    const x = v.getUint16(off), y = v.getUint16(off + 2);
    const len = v.getUint32(off + 8);
    const blob = new Blob([new Uint8Array(buf, off + 12, len)], { type: "image/jpeg" });
    off += 12 + len;
    jobs.push(createImageBitmap(blob).then((bmp) => ({ x, y, bmp })));
  }
  for (const { x, y, bmp } of await Promise.all(jobs)) {
    ctx2d.drawImage(bmp, x, y);
    bmp.close();
  }
  stats.frames++;
}

// ---------- video: H.264 via WebCodecs ----------
// Message: u8 tag | u8 flags (1 = key) | u16 w | u16 h | Annex B access unit
let decoder = null, needKey = true, keyRequested = false, chunkTs = 0;

function setupDecoder() {
  closeDecoder();
  decoder = new VideoDecoder({
    output: (frame) => {
      if (canvas.width !== frame.displayWidth || canvas.height !== frame.displayHeight) {
        canvas.width = frame.displayWidth;
        canvas.height = frame.displayHeight;
      }
      ctx2d.drawImage(frame, 0, 0);
      frame.close();
      stats.frames++;
    },
    error: (e) => {
      console.warn("decoder error", e);
      setupDecoder();
      requestKey();
    },
  });
  decoder.configure({ codec: H264_CODEC, optimizeForLatency: true });
  needKey = true;
}

function closeDecoder() {
  if (decoder && decoder.state !== "closed") decoder.close();
  decoder = null;
}

function requestKey() {
  if (keyRequested) return;
  keyRequested = true;
  needKey = true;
  videoReq(); // any request makes the host send an IDR frame
}

function h264Frame(buf) {
  if (!decoder || decoder.state !== "configured") return;
  const key = new Uint8Array(buf, 1, 1)[0] === 1;
  if (key) keyRequested = false;
  if (needKey && !key) return;
  if (decoder.decodeQueueSize > 4) { requestKey(); return; }
  needKey = false;
  decoder.decode(new EncodedVideoChunk({
    type: key ? "key" : "delta",
    timestamp: (chunkTs++) * 1000,
    data: new Uint8Array(buf, 6),
  }));
}

// ---------- input ----------
function norm(e) {
  const r = canvas.getBoundingClientRect();
  return {
    x: Math.min(1, Math.max(0, (e.clientX - r.left) / r.width)),
    y: Math.min(1, Math.max(0, (e.clientY - r.top) / r.height)),
  };
}
function input(ev) { if (!viewOnly) send("input", ev); }

let lastMove = null, moveQueued = false;
canvas.addEventListener("mousemove", (e) => {
  lastMove = norm(e);
  if (!moveQueued) {
    moveQueued = true;
    requestAnimationFrame(() => { moveQueued = false; input({ t: "move", d: display, ...lastMove }); });
  }
});
canvas.addEventListener("mousedown", (e) => {
  e.preventDefault();
  canvas.focus();
  input({ t: "move", d: display, ...norm(e) });
  input({ t: "down", b: e.button });
});
canvas.addEventListener("mouseup", (e) => { e.preventDefault(); input({ t: "up", b: e.button }); });
canvas.addEventListener("contextmenu", (e) => e.preventDefault());

let wheelAccX = 0, wheelAccY = 0;
canvas.addEventListener("wheel", (e) => {
  e.preventDefault();
  const scale = e.deltaMode === 1 ? 1 : e.deltaMode === 2 ? 10 : 1 / 100;
  wheelAccX += e.deltaX * scale;
  wheelAccY += e.deltaY * scale;
  const dx = Math.trunc(wheelAccX), dy = Math.trunc(wheelAccY);
  if (dx || dy) { wheelAccX -= dx; wheelAccY -= dy; input({ t: "wheel", dx, dy }); }
}, { passive: false });

function onKey(e, down) {
  if ($("session").hidden || document.activeElement !== canvas) return;
  e.preventDefault();
  if (down) held.add(e.code); else held.delete(e.code);
  input({ t: down ? "keydown" : "keyup", code: e.code });
}
document.addEventListener("keydown", (e) => onKey(e, true));
document.addEventListener("keyup", (e) => onKey(e, false));

function releaseAll() {
  for (const code of held) input({ t: "keyup", code });
  held.clear();
}
window.addEventListener("blur", releaseAll);
canvas.addEventListener("blur", releaseAll);
