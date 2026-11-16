// Small DOM helpers shared by the views.
"use strict";

export const $ = (id) => document.getElementById(id);

// el("li", {class: "x", onclick: f}, child, "text", ...)
export function el(tag, attrs = {}, ...children) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else if (k === "class") n.className = v;
    else n.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children) if (c != null) n.append(c);
  return n;
}

export function icon(name) {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  const use = document.createElementNS(ns, "use");
  use.setAttribute("href", "#i-" + name);
  svg.append(use);
  return svg;
}

export function show(viewId) {
  for (const v of document.querySelectorAll(".view")) v.hidden = v.id !== viewId;
}

export function toast(msg, kind = "") {
  const t = el("div", { class: "toast " + kind, role: kind === "error" ? "alert" : "status" }, msg);
  $("toasts").append(t);
  setTimeout(() => t.remove(), kind === "error" ? 7000 : 4000);
}

export function errText(e) {
  return (e && (e.message || e.toString())) || "unknown error";
}

export function fmtSize(n) {
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 1 : 0)} ${u[i]}`;
}

export function osIcon(os) {
  return icon({ windows: "windows", macOS: "apple", darwin: "apple", linux: "linux" }[os] || "monitor");
}

// Per-viewer conveniences only; never required to work.
export const store = {
  get(k, d) { try { const v = localStorage.getItem(k); return v == null ? d : JSON.parse(v); } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, JSON.stringify(v)); } catch { /* private mode etc. */ } },
};

// Human text for host refusal codes (proto.Code*).
export function refusalText(code, fallback) {
  return {
    denied: "This device isn't allowed to connect. Ask the owner of that computer to add your tailnet login.",
    auth_failed: "Wrong password or authenticator code.",
    locked: "Too many failed attempts. Wait a while, then try again.",
    busy: "That computer is already showing an approval prompt to someone else.",
    version: "The two computers run incompatible versions of rmt-desk. Update both.",
  }[code] || fallback;
}
