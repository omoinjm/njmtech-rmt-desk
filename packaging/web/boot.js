// Web viewer bootstrap: start the WebAssembly Tailscale node (cmd/rmt-web),
// then the normal UI, which finds globalThis.rmtWeb and uses it.
"use strict";

const note = document.createElement("p");
note.style.cssText = "font:15px system-ui,sans-serif;text-align:center;margin-top:30vh;opacity:.7";
note.textContent = "Loading rmt-desk (about 8 MB, cached after the first visit)…";
document.body.prepend(note);

try {
  const go = new Go();
  let inst;
  try {
    inst = await WebAssembly.instantiateStreaming(fetch("rmt-web.wasm"), go.importObject);
  } catch {
    // Server without the application/wasm type.
    const buf = await (await fetch("rmt-web.wasm")).arrayBuffer();
    inst = await WebAssembly.instantiate(buf, go.importObject);
  }
  const ready = new Promise((r) => addEventListener("rmtweb-ready", r, { once: true }));
  go.run(inst.instance);
  await ready;
  note.remove();
  await import("./app.js");
} catch (e) {
  note.textContent = "Could not start: " + (e?.message || e);
}
