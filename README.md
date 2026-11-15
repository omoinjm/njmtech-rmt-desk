# rmt-desk

A self-hosted, AnyDesk-style remote desktop for **your own devices**, running over
your Tailscale tailnet. No public ports, no third-party relay. Windows / Linux / macOS.

- `rmtd` — host agent. Embeds Tailscale (`tsnet`) so it is its own tailnet node
  (`tag:rmt-host`); Tailscale does not need to be installed.
- `rmt` — viewer (CLI now; Wails GUI next).

## Status
| Phase | Scope | State |
|---|---|---|
| 0 | REA research — see `docs/research/` | todo |
| 1 | tsnet node, WhoIs allowlist, handshake, yamux streams, `rmt list/ping` | **done** |
| 2 | Screen capture + JPEG tiles + input injection, Wails viewer | next |
| 3 | Unattended service, password/TOTP, attended consent | |
| 4 | Clipboard + file transfer | |
| 5 | H.264/WebCodecs, multi-monitor, Wayland, signed releases | |

## Security model
1. **Network:** listens only on the tailnet via tsnet. Restrict with a Tailscale grant (below).
2. **Identity:** each connection's peer is resolved with `WhoIs`; only allowlisted logins/tags
   are admitted (deny by default). In-band claims are never trusted for identity.
3. **Audit:** connects, denials and disconnects are appended to `audit.log` (0600).
4. Coming: unattended password (argon2id) + TOTP, host consent prompt, per-session permissions.

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

## Quick start
```sh
go build -o bin/ ./cmd/...

# on the machine to control
./bin/rmtd allow you@example.com
TS_AUTHKEY=tskey-auth-... ./bin/rmtd up     # tagged, pre-approved key; only needed first run

# on your laptop
./bin/rmt list
./bin/rmt ping rmt-<hostname>
```

## Develop
```sh
go test ./...
```
