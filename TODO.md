# TODO

## Blocked on a purchase
- [ ] **Windows code signing (SSL.com eSigner).** Until builds are signed, Smart App
  Control blocks the installer on PCs where it's on (no per-app override), and
  SmartScreen warns everywhere else. The release workflow is already wired up
  (`packaging/windows/sign.ps1`, `.github/workflows/release.yml`); what's left:
  1. Buy an SSL.com code-signing certificate with eSigner (OV for a business, IV for
     an individual), and complete their validation.
  2. Enroll it in eSigner; keep the TOTP **secret key text** shown with the QR code.
  3. Check the plan's signing allowance (about 12 signatures per release).
  4. Add repo secrets `ES_USERNAME`, `ES_PASSWORD`, `ES_CREDENTIAL_ID`, `ES_TOTP_SECRET`.
  5. Push a test tag (e.g. `v0.2.0-rc1`) and install the signed setup on the PC that
     Smart App Control blocked.
- [ ] **macOS signing + notarization** (Apple Developer Program). Workflow is ready;
  needs the `MACOS_*` / `APPLE_*` secrets. Until then, `.dmg` builds are ad-hoc signed.

## Needs a GitHub repo
- [ ] Create the GitHub repo (private is fine) and push; CI and releases only run there.
- [ ] First CI run: confirms the app compiles with the real Wails tags on Linux/macOS.
- [ ] First release run: confirms the macOS `.dmg`, Linux `.deb`/`.rpm`/AppImage build.

## Testing
- [ ] Windows app end to end on two PCs: "Allow remote access" (UAC + wizard), connect,
  host-side Disconnect, upgrade-in-place, uninstall removes the service.
- [ ] macOS: app, LaunchAgent install, Screen Recording/Accessibility flow.
- [ ] Linux: app + systemd user service from the `.deb`.

## Features (later)
- [ ] Native Wayland (PipeWire portal), lock-screen access, DXGI capture.
- [ ] Consent prompt inside the app instead of the native dialog.
- [ ] REA research phase (`docs/research/`).
