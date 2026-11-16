# rmt-desk

A self-hosted, AnyDesk-style remote desktop for **your own devices**, running over
your Tailscale tailnet. No public ports, no third-party relay. Windows / Linux / macOS.

- **rmt-desk** — the desktop app (Windows, macOS, Linux). Connect to your devices, and
  turn on *Allow remote access* for this one. Installs with a normal installer.
- `rmtd` — host agent (the app installs it as a background service). Embeds Tailscale
  (`tsnet`) so it is its own tailnet node (`tag:rmt-host`); Tailscale does not need to be
  installed.
- `rmt` — command-line viewer. `rmt` opens the same UI in your browser (127.0.0.1,
  one-time token); `rmt list` / `rmt ping` for scripts.

## Status
| Phase | Scope | State |
|---|---|---|
| 0 | REA research — see `docs/research/` | todo |
| 1 | tsnet node, WhoIs allowlist, handshake, yamux streams, `rmt list/ping` | **done** |
| 2 | Screen capture + dirty-tile JPEG video + mouse/keyboard, browser viewer | **done** (needs on-device testing) |
| 3 | Service install, Windows session helper, password (argon2id) + TOTP, consent prompt, lockout, permissions | **done** (needs on-device testing) |
| 4 | Clipboard sync + file transfer (sandboxed exchange folder, SHA-256 verified) | **done** (needs on-device testing) |
| 5 | H.264 (OpenH264 + WebCodecs), 6× faster Linux capture, signed release pipeline | **done** (H.264 encoder verified with the real library) |
| 6 | Desktop app (Wails) + installers (Windows setup, macOS .dmg, .deb/.rpm/AppImage), new UI, "This device" with kill switch | **done** (Windows installer built; macOS/Linux packages built by CI only) |
| — | Native Wayland (PipeWire portal), lock-screen access, DXGI capture | future |

## Security model
1. **Network:** listens only on the tailnet via tsnet. Restrict with a Tailscale grant (below).
2. **Identity:** each connection's peer is resolved with `WhoIs`; only allowlisted logins/tags
   are admitted (deny by default). In-band claims are never trusted for identity.
3. **Session admission** (after identity), per `rmtd mode`:
   - `attended` — a native Allow/Deny dialog on the host (defaults to Deny after 30 s).
   - `unattended` — password (argon2id) and, if enrolled, a TOTP code; no prompt.
   - `both` (default) — a correct password admits; with no password the host user is asked.
   - Without a password set, the host is always attended-only.
   - 5 failed attempts (or declined prompts) per peer → lockout, 1 min doubling to 1 h.
     TOTP codes can't be replayed. Only one consent dialog at a time.
4. **Permissions:** `view`, `input`, `clipboard`, `files` granted per session (config
   `access.permissions`), enforced by the host on every stream. One controller at a time.
5. **Audit:** connects, refusals, every stream opened (video/input/clipboard/files) and
   disconnects are appended to `audit.log` (0600).
6. **State protection:** the state dir (node key, config, logs) is 0700 / on Windows a
   protected DACL limited to SYSTEM + Administrators.

### Recommended tailnet policy
```jsonc
{
  "tagOwners": {
    "tag:rmt-host":   ["autogroup:admin"],
    "tag:rmt-viewer": ["autogroup:admin"]
  },
  "grants": [
    { "src": ["you@example.com", "tag:rmt-viewer"], "dst": ["tag:rmt-host"], "ip": ["tcp:7070"] }
  ]
}
```
Also enable **Tailnet Lock** so a compromised control plane can't add nodes.

## Install (easy)
1. **Tailscale account** (free): sign in once at <https://login.tailscale.com>. You don't
   need the Tailscale app on any device. In the admin console, open **Access controls** and
   add (this lets your devices register as rmt-desk hosts):
   ```json
   "tagOwners": { "tag:rmt-host": ["autogroup:admin"] },
   ```
2. **Install rmt-desk** on each device from the release page:
   Windows `…_windows_amd64_setup.exe` · macOS `…_macos_universal.dmg` ·
   Linux `…_linux_amd64.deb` / `.rpm` (the `.AppImage` can connect to others but can't be a host).
3. **Open rmt-desk → Sign in with Tailscale.** Your devices that allow remote access
   show up under *Connect to a device*.
4. **On a computer you want to reach**, flip *This device → Allow remote access*. A short
   wizard asks who may connect (pre-filled with you), whether someone must click Allow or a
   password lets you in, and signs the computer in (a second Tailscale sign-in, as
   `tag:rmt-host`). Windows asks for administrator approval once.

The person at a host always sees *This computer is being controlled by …* in the app, with
a **Disconnect** button.

## Quick start (command line)
```sh
go build -o bin/ ./cmd/...

# --- on the machine to control ---
./bin/rmtd allow you@example.com           # who may connect (tailnet login or tag:...)
TS_AUTHKEY=tskey-auth-... ./bin/rmtd up    # first run: joins the tailnet; Ctrl-C when it says "serving"

# optional: unattended access
./bin/rmtd passwd                          # set a password (>= 10 chars)
./bin/rmtd totp enable                     # + authenticator app code

# run it permanently
./bin/rmtd install && ./bin/rmtd start     # Windows: elevated prompt; state in %ProgramData%\rmt-desk

# --- on your laptop ---
./bin/rmt                                  # opens the viewer in your browser
./bin/rmt ping -p rmt-<hostname>           # CLI check (prompts for the password)
```

The UI is the same in the app and in `rmt`'s browser tab (`internal/viewer/web/static`).
In the app it's served by the native web view (WebView2 on Windows, WKWebView on macOS,
WebKitGTK on Linux); video still flows over a loopback websocket that only the app's page
can open (exact origin + a per-launch token).

## Platform notes
| OS | Capture | Input | Service | Consent dialog |
|---|---|---|---|---|
| Windows | GDI | `SendInput` (scancodes) | LocalSystem service + per-session **helper** launched with the console user's token | `MessageBoxTimeout` |
| macOS | ScreenCaptureKit | CGEvent | per-user LaunchAgent | `osascript` dialog |
| Linux | X11 MIT-SHM, persistent (XWayland ok) | XTEST | systemd **user** unit bound to `graphical-session.target` | `zenity` / `kdialog` |

How the Windows split works: a service in session 0 can't see the desktop, so `rmtd`
keeps the tailnet, allowlist, password checks and audit log in the service and launches
`rmtd helper` inside the active console session (as the logged-on user, not SYSTEM),
talking to it over inherited stdio pipes. The helper is restarted if it crashes or when
the console session changes (user switch).

Known limitations (by design for now, least privilege):
- **Windows:** no access at the logon/lock screen or to UAC prompts/elevated windows (UIPI).
- **macOS/Linux:** a user must be logged in to the desktop; macOS needs Screen Recording +
  Accessibility granted to `rmtd` (the setup wizard opens both panes); Linux needs
  X11/XWayland (no native Wayland yet) and a glibc distro (the runtime codec loader needs
  the system dynamic loader; not Alpine/musl).
- **Linux app:** WebKitGTK may not decode H.264 (WebCodecs); the app then uses JPEG tiles.
- **A host signs in twice** (once as you for the app, once as `tag:rmt-host` for the
  service). That's deliberate: a host's tag identity can't be used to control other hosts.

## Video
Two codecs, negotiated per session (toolbar → Codec):
- **JPEG tiles** (always available): only changed 64×64 tiles are sent. Crisp text, higher bandwidth.
- **H.264** (optional, much lower bandwidth, smooth motion): Cisco's OpenH264 encodes on the
  host, the browser decodes with WebCodecs. Install on each host with `rmtd codec install`.
  For licensing reasons the binary is downloaded **from Cisco on your machine** (Cisco covers
  the H.264 patent royalties only for that). It's pinned to a SHA-256 and re-verified
  before every load; a mismatching file is refused. Falls back to JPEG tiles automatically
  if the codec is missing or the browser lacks WebCodecs. 64-bit hosts only.
  *OpenH264 Video Codec provided by Cisco Systems, Inc.*

## Clipboard & files
- **Clipboard** (text, ≤ 1 MiB): synced both ways between the viewer computer's clipboard and
  the host's while the session is open (toggle in the toolbar). Only changes made *after*
  connecting are sent, so connecting never leaks what was already copied on either side.
- **Files**: one exchange folder on the host, `~/Downloads/rmt-desk`. From the viewer you can
  send files into it (button or drag-and-drop onto the screen) and download files from it,
  and nothing else: no paths, subfolders or symlinks; names are sanitized; uploads never
  overwrite (`name (1).ext`); every transfer is SHA-256 verified end to end and a failed
  upload leaves nothing behind. Default per-file limit 4 GiB.
- Backends: Windows Win32 clipboard; Linux X11 `CLIPBOARD` selection (pure Go, works on
  XWayland/WSLg); macOS `pbcopy`/`pbpaste`.

Keys are sent as physical key positions (`KeyboardEvent.code`), so the *host's*
keyboard layout decides the character. Held keys/buttons are released automatically if
the session drops. Only one viewer may control input at a time.

## Releases
Push a `v*` tag: `.github/workflows/release.yml` builds natively on Linux/Windows/macOS
(amd64 + arm64): the app installers (Windows setup `.exe`, macOS universal `.dmg`, Linux
`.deb`/`.rpm`/`.AppImage`) and the command-line archives. It Authenticode-signs Windows
files with **SSL.com eSigner** (the executables, the installer and its uninstaller, which
Smart App Control requires) and codesigns + notarizes macOS builds, when the secrets
listed at the top of the workflow are configured
(otherwise macOS builds are ad-hoc signed: right-click → Open the first time), and
publishes everything with
`SHA256SUMS`, a Sigstore keyless signature (`SHA256SUMS.sigstore.json`) and SLSA build
provenance. Verify a download:
```sh
sha256sum -c SHA256SUMS --ignore-missing
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp 'https://github.com/<owner>/rmt-desk/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
gh attestation verify rmt-desk_*.tar.gz --repo <owner>/rmt-desk
```

## Develop
```sh
go test -race ./...
# Desktop app (needs the Wails CLI; Linux also libgtk-3-dev + libwebkit2gtk-4.1-dev):
go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.1
cd cmd/rmt-desk && wails build -skipbindings -tags webkit2_41          # Linux
cd cmd/rmt-desk && wails build -platform windows/amd64 -nsis -skipbindings
                   # (put rmtd.exe in cmd/rmt-desk/build/bin first; needs makensis)
# UI in a headless browser against a fake host, with the app's bindings mocked:
go test -tags uicheck -run TestUIServe ./internal/viewer/web   # prints the URL; ?s=home|session|setup…
# H.264 end-to-end check (encodes with the real Cisco library, decodes with ffmpeg):
RMT_OPENH264_LIB=/path/to/libopenh264-2.6.0-linux64.8.so go test -run FFmpeg -v ./internal/codec/h264
```
