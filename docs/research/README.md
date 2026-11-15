# Research phase (REA)

[REA](https://github.com/morluto/rea) is an MCP server that lets your coding agent
decompile and inspect binaries (via Ghidra/Hopper/IDA) and JS/.NET apps.

## Rules of engagement
- **Do analyse:** RustDesk release binaries (AGPL-3.0 — we study techniques, we do
  **not** copy code into this MIT/own-licensed project), our own `rmtd`/`rmt` builds.
- **Do not analyse:** AnyDesk, TeamViewer, or other proprietary clients — their
  EULAs prohibit reverse engineering. This project is a clean-room implementation.

## Setup
```sh
npx rea-agents setup     # registers the MCP + skill with Claude Code; restart the agent
```
Ghidra (free) is the recommended native engine.

## Questions to answer (write findings in separate notes in this folder)
1. **Windows unattended access** — how does a session-0 service capture and inject
   into the interactive desktop? Confirm the `WTSQueryUserToken` +
   `CreateProcessAsUser` helper pattern, how the helper follows session switches
   (lock screen / UAC secure desktop / fast user switching), and IPC hardening.
2. **macOS TCC** — how Screen Recording & Accessibility permission state is
   detected and how the user is guided to grant it; daemon vs agent split.
3. **Linux Wayland** — xdg-desktop-portal ScreenCast + RemoteDesktop portal
   flow, persisting the portal "restore token" for unattended use.
4. **Capture/encode pipeline** — dirty-rect detection, frame pacing, H.264 encoder
   settings for low latency.
5. **Self-audit (later)** — run REA on our release binaries: no embedded auth keys,
   no debug endpoints, listener only on tailnet.
