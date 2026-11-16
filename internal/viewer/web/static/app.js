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
const canvas = $("screen");
const ctx2d = canvas.getContext("2d");
const held = new Set(); // key codes held in the browser

function status(s) { $("status").textContent = s; }

function send(type, data) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type, data }));
}

function videoReq() {
  send("video", { display, fps: +$("fps").value, quality: +$("quality").value });
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
    if (typeof ev.data !== "string") { enqueueFrame(ev.data); return; }
    const m = JSON.parse(ev.data);
    switch (m.type) {
      case "hello":
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
        break;
      }
      case "status":
        status(m.error);
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

function disconnect() {
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
