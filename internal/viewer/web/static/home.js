// Home: sign-in gate (app), "This device" card (app), device list.
"use strict";

import { api, isApp } from "./platform.js";
import { $, el, icon, show, toast, errText, osIcon, store } from "./ui.js";
import { connect } from "./session.js";
import { openSetup } from "./setup.js";

let info = { hostSupported: false };
let hostsTimer = null, deviceTimer = null;
let lastHosts = [];

export async function start(i) {
  info = i;
  if (isApp) await signInGate();
  enterHome();
}

// ---------- sign-in (app only) ----------
async function signInGate() {
  let st = await api.viewerState().catch(() => null);
  if (st?.Running) { setMe(st); return; }
  show("signin");
  const msg = $("signin-status");
  $("signin-btn").onclick = async () => {
    try {
      await api.signIn();
      msg.textContent = "Finish signing in in your browser. This window continues automatically.";
    } catch (e) { msg.textContent = errText(e); }
  };
  for (;;) {
    st = await api.viewerState().catch((e) => ({ Error: errText(e) }));
    if (st.Running) break;
    $("signin-btn").disabled = !st.AuthURL;
    if (st.Error) msg.textContent = st.Error;
    else if (!st.AuthURL) msg.textContent = "Starting…";
    await sleep(1000);
  }
  setMe(st);
  toast("Signed in");
}

function setMe(st) {
  if (!st?.LoginName) return;
  $("me").hidden = false;
  $("me").querySelector("span").textContent = st.LoginName;
}

export function enterHome() {
  show("home");
  $("device").hidden = !isApp;
  renderRecent();
  loadHosts();
  clearInterval(hostsTimer);
  hostsTimer = setInterval(() => { if (!$("home").hidden) loadHosts(true); }, 5000);
  if (isApp) {
    refreshDevice();
    clearInterval(deviceTimer);
    deviceTimer = setInterval(() => { if (!$("home").hidden) refreshDevice(); }, 3000);
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ---------- device list ----------
async function loadHosts(quiet) {
  const ul = $("hosts");
  if (!quiet) { ul.textContent = ""; ul.append(el("li", { class: "empty" }, "Looking for devices…")); }
  try {
    lastHosts = await api.listHosts() || [];
    renderHosts();
  } catch (e) {
    if (!quiet) { ul.textContent = ""; ul.append(el("li", { class: "empty" }, "Error: " + errText(e))); }
  }
}

function renderHosts() {
  const ul = $("hosts");
  const q = $("manual-host").value.trim().toLowerCase();
  const hosts = lastHosts.filter((h) => !q || h.HostName.toLowerCase().includes(q) || (h.IP || "").includes(q));
  ul.textContent = "";
  if (!hosts.length) {
    ul.append(el("li", { class: "empty" }, lastHosts.length
      ? "No device matches. Press Connect to try that name anyway."
      : "No devices yet. Turn on remote access on another computer (tagged tag:rmt-host), or type a host name above."));
    return;
  }
  hosts.sort((a, b) => (b.Online - a.Online) || a.HostName.localeCompare(b.HostName));
  for (const h of hosts) {
    ul.append(el("li", { class: h.Online ? "" : "offline", title: h.DNSName || h.HostName, onclick: () => connect(h.HostName) },
      el("span", { class: "os" }, osIcon(h.OS)),
      el("span", { class: "txt" },
        el("div", { class: "name" }, h.HostName),
        el("div", { class: "meta" }, el("span", { class: "dot" + (h.Online ? " on" : "") }),
          `${h.Online ? "Online" : "Offline"} · ${h.IP || "?"}`))));
  }
}

$("refresh").onclick = () => loadHosts();
$("manual-host").oninput = renderHosts;
$("manual").onsubmit = (e) => {
  e.preventDefault();
  const h = $("manual-host").value.trim();
  if (h) connect(h);
};

function renderRecent() {
  const r = store.get("rmt.recent", []);
  $("recent-wrap").hidden = !r.length;
  const ul = $("recent");
  ul.textContent = "";
  for (const h of r) ul.append(el("li", { onclick: () => connect(h), title: "Connect to " + h }, icon("monitor"), h));
}

// ---------- this device (app only) ----------
let busy = false;

async function refreshDevice() {
  if (busy) return;
  const body = $("device-body");
  const tog = $("host-toggle");
  if (!info.hostSupported) {
    tog.disabled = true;
    body.textContent = "";
    body.append(el("p", { class: "muted" }, info.hostNote || "Remote access to this computer isn't available."));
    return;
  }
  let v;
  try { v = await api.hostStatus(); } catch (e) { return; }
  const on = v.service !== "not installed";
  tog.checked = on;
  tog.disabled = false;
  body.textContent = "";
  if (!on) {
    body.append(
      el("p", { class: "muted" }, "Remote access is off. Turn it on to reach this computer from your other devices."));
    return;
  }
  const st = v.status;
  const name = st?.tailnetName || st?.hostName || "this computer";
  body.append(el("div", { class: "dev-name" }, name));
  if (!st) {
    body.append(el("p", { class: "muted" }, v.service === "stopped" ? "The background service is stopped." : "Starting…"));
  } else {
    const modeText = { attended: "Ask each time", unattended: "Password", both: "Password or ask" }[st.mode] || st.mode;
    body.append(el("dl", { class: "kv" },
      el("dt", {}, "Status"), el("dd", {}, el("span", { class: "badge" + (st.online ? " ok" : "") }, st.online ? "Online" : "Offline")),
      el("dt", {}, "Address"), el("dd", {}, st.ip || "-"),
      el("dt", {}, "Approval"), el("dd", {}, modeText + (st.totpSet ? " + code" : "")),
      el("dt", {}, "Allowed"), el("dd", {}, (st.allowed || []).join(", ") || "nobody yet")));
    for (const s of st.sessions || []) {
      const since = new Date(s.since).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
      body.append(el("div", { class: "controlled", role: "alert" },
        el("strong", {}, "This computer is being controlled"),
        el("span", {}, `${s.peer} since ${since}`),
        el("button", { class: "danger", type: "button", onclick: disconnectAll }, icon("x"), "Disconnect")));
    }
  }
  body.append(el("div", { class: "row" },
    el("button", { type: "button", onclick: () => enable() }, icon("settings"), "Settings")));
}

async function disconnectAll() {
  try {
    const n = await api.disconnectSessions();
    toast(n ? "Disconnected" : "Nobody was connected");
  } catch (e) { toast(errText(e), "error"); }
  refreshDevice();
}

async function enable() {
  busy = true;
  try {
    const how = await api.enableHost();
    if (how === "inprocess") { openSetup({ inProcess: true, onDone: enterHome }); return; }
    toast("Finish setup in the window that just opened.");
  } catch (e) {
    toast(errText(e), "error");
  } finally {
    busy = false;
    refreshDevice();
  }
}

$("host-toggle").onchange = async (e) => {
  const want = e.target.checked;
  e.target.checked = !want; // reflect real state until confirmed
  if (want) return enable();
  if (!(await api.confirm("Turn off remote access?", "Other devices won't be able to reach this computer. Your settings are kept."))) return;
  busy = true;
  try {
    await api.disableHost();
    toast("Remote access turned off");
  } catch (e) { toast(errText(e), "error"); }
  busy = false;
  setTimeout(refreshDevice, 800);
};
