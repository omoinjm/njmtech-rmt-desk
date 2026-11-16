"use strict";

const $ = (id) => document.getElementById(id);

// ---------- Home: host list ----------
async function loadHosts() {
  const ul = $("hosts");
  ul.textContent = "Loading…";
  try {
    const res = await fetch("/api/hosts");
    if (!res.ok) throw new Error(await res.text());
    const hosts = await res.json();
    ul.textContent = "";
    if (!hosts.length) {
      ul.textContent = "No hosts found. Check your tailnet ACLs, or type a host below.";
      return;
    }
    for (const h of hosts) {
      const li = document.createElement("li");
      const dot = document.createElement("span");
      dot.className = "dot" + (h.Online ? " on" : "");
      const name = document.createElement("span");
      name.className = "name";
      name.textContent = h.HostName;
      const meta = document.createElement("span");
      meta.className = "meta";
      meta.textContent = `${h.OS || "?"} · ${h.IP}`;
      li.append(dot, name, meta);
      li.onclick = () => connect(h.HostName);
      ul.append(li);
    }
  } catch (e) {
    ul.textContent = "Error: " + e.message;
  }
}
$("refresh").onclick = loadHosts;
$("manual").onsubmit = (e) => {
  e.preventDefault();
  const h = $("manual-host").value.trim();
  if (h) connect(h);
};

// ---------- Session ----------
let ws = null;
let screenW = 0, screenH = 0, display = 0;
let viewOnly = false;
let currentHost = null;
let creds = { password: "", totp: "" }; // kept in memory only, per host
let sid = null;      // bridge session id, for file endpoints
let granted = [];
const canvas = $("screen");
const ctx2d = canvas.getContext("2d");
const held = new Set(); // key codes held in the browser

function status(s) { $("status").textContent = s; }

function send(type, data) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type, data }));
}

// ---------- Codec negotiation ----------
// Constrained Baseline, level 5.1 upper bound (OpenH264 output fits).
const H264_CODEC = "avc1.42E033";
let h264Supported = false;
let currentCodec = "tiles";
(async () => {
  try {
    if ("VideoDecoder" in window) {
      const r = await VideoDecoder.isConfigSupported({ codec: H264_CODEC, optimizeForLatency: true });
      h264Supported = !!r.supported;
    }
  } catch { h264Supported = false; }
})();

function codecs() {
  return h264Supported && $("codec").value === "auto" ? ["h264", "tiles"] : ["tiles"];
}

function videoReq() {
  send("video", { display, fps: +$("fps").value, quality: +$("quality").value, codecs: codecs() });
}

function connect(host) {
  if (host !== currentHost) creds = { password: "", totp: "" };
  currentHost = host;
  $("auth").hidden = true;
  $("home").hidden = true;
  $("session").hidden = false;
  $("title").textContent = host;
  status("connecting…");
  viewOnly = false;
  if (ws) ws.close();
  const sock = new WebSocket(`ws://${location.host}/ws?host=${encodeURIComponent(host)}`);
  ws = sock;
  sock.binaryType = "arraybuffer";
  sock.onopen = () => {
    send("connect", creds);
    creds.totp = ""; // one-time code: never reuse
  };
  sock.onmessage = (ev) => {
    if (ws !== sock) return; // stale socket from a previous attempt
    if (typeof ev.data !== "string") {
      // 1-byte codec tag: 0 = JPEG tiles, 1 = H.264
      const tag = new Uint8Array(ev.data, 0, 1)[0];
      if (tag === 0) enqueueFrame(ev.data.slice(1));
      else if (tag === 1) h264Frame(ev.data);
      return;
    }
    const m = JSON.parse(ev.data);
    switch (m.type) {
      case "hello":
        sid = m.data.sid;
        granted = m.data.granted || [];
        $("files-btn").hidden = !granted.includes("files");
        $("clip").parentElement.hidden = !granted.includes("clipboard");
        if (!$("clip").checked) send("clipboard", { enabled: false });
        status(`connected · ${m.data.os} · ${m.data.identity}`);
        videoReq();
        canvas.focus();
        break;
      case "info": {
        const sel = $("display");
        sel.textContent = "";
        for (const d of m.data.displays) {
          const o = document.createElement("option");
          o.value = d.index;
          o.textContent = `${d.index + 1}: ${d.width}×${d.height}`;
          sel.append(o);
        }
        display = m.data.current;
        sel.value = display;
        if (m.data.codec !== currentCodec || m.data.codec === "h264") {
          currentCodec = m.data.codec;
          if (currentCodec === "h264") setupDecoder(); else closeDecoder();
        }
        $("codec").title = "Current: " + (currentCodec === "h264" ? "H.264" : "JPEG tiles");
        break;
      }
      case "status":
        status(m.error);
        break;
      case "clipboard":
        if (m.error) status(m.error);
        break;
      case "view-only":
        viewOnly = true;
        status("view-only: " + m.error);
        break;
      case "error":
        status("error: " + m.error);
        if (m.code === "auth_required" || m.code === "auth_failed") showAuth(m.error);
        break;
    }
  };
  sock.onclose = () => {
    if (ws !== sock) return;
    ws = null;
    const st = $("status").textContent;
    status((st.startsWith("error") ? st + " · " : "") + "disconnected");
  };
}

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
$("auth-ask").onclick = () => {
  creds = { password: "", totp: "" };
  connect(currentHost);
};

// ---------- Clipboard ----------
$("clip").onchange = (e) => send("clipboard", { enabled: e.target.checked });

// ---------- Files ----------
function fmtSize(n) {
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 1 : 0)} ${u[i]}`;
}

async function loadFiles() {
  const ul = $("file-list");
  if (!sid) return;
  ul.textContent = "Loading…";
  try {
    const res = await fetch(`/api/files?sid=${sid}`);
    if (!res.ok) throw new Error(await res.text());
    const l = await res.json();
    $("files-dir").textContent = l.dir;
    ul.textContent = "";
    if (!l.files.length) { ul.textContent = "Empty. Files you send appear here."; return; }
    for (const f of l.files) {
      const li = document.createElement("li");
      const a = document.createElement("a");
      a.href = `/api/download?sid=${sid}&name=${encodeURIComponent(f.name)}`;
      a.download = f.name;
      a.textContent = f.name;
      a.title = "Download to this computer";
      const size = document.createElement("span");
      size.className = "size";
      size.textContent = fmtSize(f.size);
      li.append(a, size);
      ul.append(li);
    }
  } catch (e) {
    ul.textContent = "Error: " + e.message;
  }
}

function uploadFile(file) {
  const li = document.createElement("li");
  const name = document.createElement("span");
  name.textContent = file.name;
  const state = document.createElement("span");
  state.className = "state";
  state.textContent = fmtSize(file.size);
  const bar = document.createElement("progress");
  bar.max = file.size || 1;
  bar.value = 0;
  li.append(name, state, bar);
  $("transfers").prepend(li);

  const xhr = new XMLHttpRequest();
  xhr.open("POST", `/api/upload?sid=${sid}&name=${encodeURIComponent(file.name)}`);
  xhr.setRequestHeader("X-Rmt-Upload", "1");
  xhr.upload.onprogress = (e) => { bar.value = e.loaded; };
  xhr.onload = () => {
    bar.remove();
    if (xhr.status === 200) {
      const d = JSON.parse(xhr.responseText);
      state.textContent = "sent ✓ verified";
      name.textContent = d.name;
      loadFiles();
    } else {
      const err = document.createElement("span");
      err.className = "err";
      err.textContent = xhr.responseText.trim() || `failed (${xhr.status})`;
      state.textContent = "failed";
      li.append(err);
    }
  };
  xhr.onerror = () => { bar.remove(); state.textContent = "failed (connection)"; };
  xhr.send(file);
}

$("files-btn").onclick = () => {
  const p = $("files");
  p.hidden = !p.hidden;
  if (!p.hidden) loadFiles();
};
$("files-refresh").onclick = loadFiles;
$("upload").onchange = (e) => { for (const f of e.target.files) uploadFile(f); e.target.value = ""; };

// Drag & drop onto the remote screen.
const stage = $("stage");
let dragDepth = 0;
stage.addEventListener("dragenter", (e) => {
  if (!sid || !granted.includes("files") || !e.dataTransfer.types.includes("Files")) return;
  e.preventDefault(); dragDepth++; $("drop").hidden = false;
});
stage.addEventListener("dragover", (e) => { if (!$("drop").hidden) e.preventDefault(); });
stage.addEventListener("dragleave", () => { if (--dragDepth <= 0) { dragDepth = 0; $("drop").hidden = true; } });
stage.addEventListener("drop", (e) => {
  if ($("drop").hidden) return;
  e.preventDefault(); dragDepth = 0; $("drop").hidden = true;
  $("files").hidden = false;
  for (const f of e.dataTransfer.files) uploadFile(f);
});

function disconnect() {
  closeDecoder();
  currentCodec = "tiles";
  sid = null;
  $("files").hidden = true;
  $("transfers").textContent = "";
  creds = { password: "", totp: "" };
  $("auth").hidden = true;
  releaseAll();
  if (ws) ws.close();
  ws = null;
  $("session").hidden = true;
  $("home").hidden = false;
  ctx2d.clearRect(0, 0, canvas.width, canvas.height);
  loadHosts();
}
$("disconnect").onclick = disconnect;
$("display").onchange = (e) => { display = +e.target.value; videoReq(); };
$("quality").onchange = videoReq;
$("fps").onchange = videoReq;
$("codec").onchange = videoReq;
$("fit").onchange = (e) => $("stage").classList.toggle("fit", e.target.checked);
$("stage").classList.add("fit");

// ---------- Video: tiles frames ----------
// Frame: u16 w | u16 h | u16 n | n × (u16 x | u16 y | u16 w | u16 h | u32 len | jpeg)
let chain = Promise.resolve();
let pending = 0;
let needKeyframe = false;
function enqueueFrame(buf) {
  // Drop frames if decoding falls behind (keeps latency bounded). A dropped
  // delta frame could leave stale tiles, so request a keyframe afterwards.
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
  if (w !== screenW || h !== screenH) {
    screenW = canvas.width = w;
    screenH = canvas.height = h;
  }
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
}

// ---------- Video: H.264 via WebCodecs ----------
// Message: u8 tag | u8 flags (1 = key) | u16 w | u16 h | Annex B access unit
let decoder = null, needKey = true, keyRequested = false, chunkTs = 0;

function setupDecoder() {
  closeDecoder();
  decoder = new VideoDecoder({
    output: (frame) => {
      if (canvas.width !== frame.displayWidth || canvas.height !== frame.displayHeight) {
        screenW = canvas.width = frame.displayWidth;
        screenH = canvas.height = frame.displayHeight;
      }
      ctx2d.drawImage(frame, 0, 0);
      frame.close();
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
  if (needKey && !key) return; // can't decode deltas without a reference
  if (decoder.decodeQueueSize > 4) { requestKey(); return; } // falling behind: resync
  needKey = false;
  decoder.decode(new EncodedVideoChunk({
    type: key ? "key" : "delta",
    timestamp: (chunkTs++) * 1000,
    data: new Uint8Array(buf, 6),
  }));
}

// ---------- Input ----------
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
    requestAnimationFrame(() => {
      moveQueued = false;
      input({ t: "move", d: display, ...lastMove });
    });
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
  const scale = e.deltaMode === 1 ? 1 : e.deltaMode === 2 ? 10 : 1 / 100; // px → clicks
  wheelAccX += e.deltaX * scale;
  wheelAccY += e.deltaY * scale;
  const dx = Math.trunc(wheelAccX), dy = Math.trunc(wheelAccY);
  if (dx || dy) {
    wheelAccX -= dx; wheelAccY -= dy;
    input({ t: "wheel", dx, dy });
  }
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

loadHosts();
