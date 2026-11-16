// Host setup wizard: who may connect, how they're admitted, tailnet
// sign-in for this computer, then install and start the service.
"use strict";

import { api } from "./platform.js";
import { $, el, icon, show, toast, errText } from "./ui.js";

let step = 1;
let info = null;
let allowed = [];
let opts = { inProcess: false, onDone: () => {} };
let loggedIn = false;

export async function openSetup(o) {
  opts = { ...opts, ...o };
  show("setup");
  try {
    info = await api.setupInfo();
  } catch (e) {
    toast(errText(e), "error");
    return;
  }
  allowed = info.allowed?.length ? [...info.allowed] : (info.prefill ? [info.prefill] : []);
  loggedIn = info.loggedIn;
  const mode = info.mode || "both";
  for (const r of document.querySelectorAll("input[name=mode]")) r.checked = r.value === mode;
  $("pw1").value = $("pw2").value = "";
  $("pw-clear").checked = false;
  $("pw-clear-wrap").hidden = !info.passwordSet;
  $("pw1").placeholder = info.passwordSet ? "unchanged" : "at least 10 characters";
  $("codec-opt").checked = true;
  $("codec-opt").parentElement.hidden = info.codec;
  $("tag-snippet").textContent = info.policyHelp;
  renderAllowed();
  updatePwSection();
  goto(1);
}

function goto(n) {
  step = n;
  for (const s of document.querySelectorAll(".step")) s.hidden = +s.dataset.step !== n;
  for (const li of document.querySelectorAll("#steps li")) {
    const k = +li.dataset.step;
    li.className = k < n ? "done" : k === n ? "cur" : "";
  }
  $("wiz-back").hidden = n === 1 || n === 4;
  $("wiz-cancel").hidden = n === 4;
  const next = $("wiz-next");
  next.disabled = false;
  next.textContent = { 1: "Next", 2: "Save", 3: "Next", 4: "Turn on remote access" }[n];
  if (n === 2) updateTOTP();
  if (n === 3) renderLogin();
  if (n === 4) renderFinish();
}

// ---------- step 1: who ----------
function renderAllowed() {
  const ul = $("allow-list");
  ul.textContent = "";
  if (!allowed.length) ul.append(el("li", { class: "muted" }, "Nobody yet"));
  for (const who of allowed) {
    ul.append(el("li", {}, icon(who.startsWith("tag:") ? "shield" : "user"), who,
      el("button", { type: "button", title: "Remove", onclick: () => { allowed = allowed.filter((x) => x !== who); renderAllowed(); } }, icon("x"))));
  }
}
$("allow-form").onsubmit = (e) => {
  e.preventDefault();
  const v = $("allow-input").value.trim();
  if (!v) return;
  if (/\s/.test(v)) { toast("Logins can't contain spaces", "error"); return; }
  if (!allowed.includes(v)) allowed.push(v);
  $("allow-input").value = "";
  renderAllowed();
};

// ---------- step 2: access ----------
function currentMode() { return document.querySelector("input[name=mode]:checked")?.value || "both"; }
function updatePwSection() { $("pw-section").hidden = currentMode() === "attended"; }
for (const r of document.querySelectorAll("input[name=mode]")) r.onchange = updatePwSection;

function updateTOTP() {
  const has = info.passwordSet;
  $("totp-section").hidden = !has;
  $("totp-enroll").hidden = true;
  $("totp-state").textContent = info.totpSet
    ? "An authenticator code is required with the password."
    : "Optionally require a 6-digit code from an authenticator app as well as the password.";
  $("totp-begin").textContent = info.totpSet ? "Turn off the authenticator code" : "Set up an authenticator code";
}

let enrolling = null;
$("totp-begin").onclick = async () => {
  try {
    if (info.totpSet) {
      await api.setupTOTPDisable();
      info.totpSet = false;
      updateTOTP();
      toast("Authenticator code turned off");
      return;
    }
    enrolling = await api.setupTOTPBegin();
    $("totp-qr").src = enrolling.qr;
    $("totp-secret").textContent = enrolling.secret;
    $("totp-enroll").hidden = false;
    $("totp-code").focus();
  } catch (e) { toast(errText(e), "error"); }
};
$("totp-form").onsubmit = async (e) => {
  e.preventDefault();
  try {
    await api.setupTOTPConfirm(enrolling.secret, $("totp-code").value.trim());
    info.totpSet = true;
    enrolling = null;
    $("totp-code").value = "";
    updateTOTP();
    toast("Authenticator code enabled");
  } catch (err) { toast(errText(err), "error"); }
};

async function saveAccess() {
  const mode = currentMode();
  const pw1 = $("pw1").value, pw2 = $("pw2").value;
  const clear = $("pw-clear").checked;
  if (mode !== "attended" && !clear) {
    if (pw1 !== pw2) throw new Error("The passwords don't match.");
    if (!pw1 && !info.passwordSet) throw new Error("Choose a password, or pick “Ask me each time”.");
    if (pw1 && pw1.length < 10) throw new Error("Use at least 10 characters.");
  }
  await api.setupSave({ allowed, mode, password: mode === "attended" ? "" : pw1, clearPassword: clear });
  if (pw1) info.passwordSet = true;
  if (clear) { info.passwordSet = false; info.totpSet = false; }
  $("pw1").value = $("pw2").value = "";
  if (!$("codec-opt").parentElement.hidden && $("codec-opt").checked) {
    toast("Downloading the H.264 codec…");
    try {
      await api.setupCodec();
      info.codec = true;
      $("codec-opt").parentElement.hidden = true;
    } catch (e) { toast("H.264 download failed (JPEG video still works): " + errText(e), "error"); }
  }
}

// ---------- step 3: sign in ----------
let loginState = null;
function renderLogin() {
  const b = $("login-body");
  b.textContent = "";
  $("tag-help").hidden = true;
  if (loggedIn) {
    b.append(el("div", { class: "ok-big" }, icon("check"), "This computer is signed in."));
    $("wiz-next").disabled = false;
    return;
  }
  $("wiz-next").disabled = true;
  const btn = el("button", { class: "primary", type: "button", onclick: beginLogin }, "Sign in with Tailscale");
  b.append(btn, el("p", { id: "login-msg", class: "muted small" }, ""));
}

let loginStarted = false;
let openedURL = "";
async function beginLogin() {
  const msg = $("login-msg");
  if (loginState?.AuthURL) { api.openURL(loginState.AuthURL); return; }
  if (!loginStarted) {
    loginStarted = true;
    msg.textContent = "Starting…";
    try { await api.setupLogin(); } catch (e) { loginStarted = false; msg.textContent = errText(e); }
  }
}

if (api.on) {
  api.on("setup:login", (ev) => {
    if (step !== 3) return;
    const msg = $("login-msg");
    if (ev.state) loginState = ev.state;
    if (ev.done) {
      loggedIn = true;
      loginStarted = false;
      renderLogin();
      toast("This computer is signed in");
      return;
    }
    if (ev.tagDenied) {
      loginStarted = false;
      loginState = null;
      $("tag-help").hidden = false;
      $("login-body").textContent = ""; // "Try again" replaces the sign-in button
      return;
    }
    if (ev.error) {
      loginStarted = false;
      loginState = null;
      if (msg) msg.textContent = ev.error;
      return;
    }
    if (loginState?.AuthURL && msg) {
      msg.textContent = "Your browser opened the sign-in page. If it didn't, click the button again.";
      if (openedURL !== loginState.AuthURL) { openedURL = loginState.AuthURL; api.openURL(openedURL); }
    } else if (msg) {
      msg.textContent = "Connecting…";
    }
  });
}
$("tag-copy").onclick = () => api.copyText(info.policyHelp).then(() => toast("Copied"));
$("tag-open").onclick = () => api.openURL("https://login.tailscale.com/admin/acls/file");
$("tag-retry").onclick = () => { $("tag-help").hidden = true; renderLogin(); beginLogin(); };
$("pw-section").onsubmit = (e) => e.preventDefault();

// ---------- step 4: finish ----------
let finished = false;
function renderFinish() {
  finished = false;
  const b = $("finish-body");
  b.textContent = "";
  b.append(el("p", {}, `This computer will appear as `, el("code", {}, info.hostName),
    ` to: ${allowed.join(", ")}.`));
  b.append(el("p", { class: "muted" }, "rmt-desk runs in the background and starts with the computer. You can turn it off any time from the app."));
  $("mac-perms").hidden = true;
}

async function finish() {
  const next = $("wiz-next");
  next.disabled = true;
  next.textContent = "Starting…";
  try {
    await api.setupFinish();
  } catch (e) {
    toast(errText(e), "error");
    next.disabled = false;
    next.textContent = "Turn on remote access";
    return;
  }
  finished = true;
  const b = $("finish-body");
  b.textContent = "";
  b.append(el("div", { class: "ok-big" }, icon("check"), "Remote access is on."));
  b.append(el("p", { class: "muted" }, `Connect from another device: it's listed as ${info.hostName}.`));
  const i = await api.info();
  $("mac-perms").hidden = i.os !== "darwin";
  next.disabled = false;
  next.textContent = "Done";
}
for (const b of document.querySelectorAll("#mac-perms button")) {
  b.onclick = () => api.openPrivacySettings(b.dataset.pane).catch((e) => toast(errText(e), "error"));
}

// ---------- navigation ----------
function close() {
  if (opts.inProcess) opts.onDone();
  else api.closeWindow();
}

$("wiz-cancel").onclick = close;
$("wiz-back").onclick = () => goto(step - 1);
$("wiz-next").onclick = async () => {
  const next = $("wiz-next");
  try {
    if (step === 1) {
      if (!allowed.length) throw new Error("Add at least one login.");
      goto(2);
    } else if (step === 2) {
      next.disabled = true;
      await saveAccess();
      goto(3);
    } else if (step === 3) {
      goto(4);
    } else if (step === 4) {
      if (finished) close(); else await finish();
    }
  } catch (e) {
    toast(errText(e), "error");
    next.disabled = false;
  }
};
