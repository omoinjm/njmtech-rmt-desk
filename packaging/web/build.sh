#!/usr/bin/env bash
# Builds the web viewer (a static site) into ${1:-dist/web}: the normal UI
# plus cmd/rmt-web compiled to WebAssembly. Serve it over HTTPS (or from
# localhost): WebCodecs and the clipboard need a secure context.
set -euo pipefail
cd "$(dirname "$0")/../.."
out="${1:-dist/web}"
static=internal/viewer/web/static
rm -rf "$out" && mkdir -p "$out"

GOOS=js GOARCH=wasm go build -trimpath -ldflags "-s -w ${LDFLAGS:-}" -o "$out/rmt-web.wasm" ./cmd/rmt-web
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" packaging/web/boot.js "$out/"
cp "$static"/*.js "$static"/*.css "$out/"

# Same page as the app, with a CSP for WebAssembly + Tailscale (control and
# DERP relays over HTTPS/WSS), booting the WASM node before the UI.
csp="default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; img-src 'self' blob: data:; connect-src 'self' https: wss:; object-src 'none'; base-uri 'none'; form-action 'none'"
python3 - "$static/index.html" "$out/index.html" "$csp" <<'PY'
import re, sys
src, dst, csp = sys.argv[1:]
s = open(src).read()
s, n1 = re.subn(r'(<meta http-equiv="Content-Security-Policy" content=")[^"]*(">)', lambda m: m[1] + csp + m[2], s)
s, n2 = re.subn(r'<script type="module" src="app.js"></script>',
    '<script src="wasm_exec.js"></script>\n<script type="module" src="boot.js"></script>', s)
assert n1 == 1 and n2 == 1, "index.html layout changed"
open(dst, "w").write(s)
PY
ls -la "$out"
