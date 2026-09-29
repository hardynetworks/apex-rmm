# Hardy RMM

A self-hosted Remote Monitoring & Management platform that runs in Docker, signs users in through **Authentik SSO**, and manages **Windows, macOS and Linux** machines with a lightweight Go agent.

| | |
|---|---|
| **Remote control** | Built-in browser remote desktop written from scratch (screen capture + dirty-tile JPEG streaming + keyboard/mouse injection) with **RustDesk** as a one-click backup connection |
| **Monitoring & alerts** | Live CPU / memory / disk / network, 14-day history charts, threshold & offline policies, webhook notifications (Discord, Slack, ntfy, generic), maintenance mode |
| **Scripting & automation** | Script library (PowerShell, cmd, Bash, sh, zsh, Python), run on one/many/all devices, per-device output, cron schedules, queued runs for offline devices |
| **Ticketing / PSA** | Tickets linked to clients & devices, assignees, priorities, due dates, internal notes, time tracking, alerts → tickets |
| **Device tools** | Web terminal (real PTY / Windows ConPTY), processes (kill), services (start/stop/restart), installed software, reboot/shutdown, agent self-update |
| **Multi-tenant** | Clients → sites → devices, per-client install links |
| **Security** | Authentik OIDC with PKCE, role mapping from Authentik groups (admin / technician / viewer), full audit log, per-device secrets |

![Dashboard](docs/screenshots/dashboard.png)

| Remote control | Device | Terminal |
|---|---|---|
| ![](docs/screenshots/remote-control.png) | ![](docs/screenshots/device.png) | ![](docs/screenshots/terminal.png) |

---

## Architecture

```
 Browser ──HTTPS/WSS──┐                         ┌── hbbs / hbbr (RustDesk server, ports 21115-21119)
                      ▼                         │
              ┌──────────────────┐              │
              │   hardy-server   │  Postgres    │
              │  Go API + React  │◄──────────►  db
              │  dashboard + hub │
              └──────────────────┘
                 ▲            ▲
    WSS control  │            │  WSS screen stream (per session)
                 │            │
        ┌────────┴───┐   ┌────┴──────────────┐
        │ hardy-agent│──►│ hardy-agent       │   spawned in the logged-in user's
        │ (service)  │   │ desktop helper    │   session (SYSTEM on Windows)
        └────────────┘   └───────────────────┘
```

* **Agents only make outbound connections** (HTTPS/WSS to the server) — no inbound ports on endpoints, works behind NAT.
* The server relays terminal and desktop traffic; nothing is stored.
* One static Go binary per platform, no runtime dependencies (built with `CGO_ENABLED=0`, including macOS).

## Quick start

### 1. DNS

| Record | Points to | Notes |
|---|---|---|
| `remote.hardyvpn.online` | your Docker host | Can be Cloudflare-proxied (WebSockets work) |
| `rustdesk.hardyvpn.online` | your Docker host | **DNS only** (grey cloud) – RustDesk uses raw TCP/UDP |

Open/forward TCP **21115-21119** and UDP **21116** to the host for RustDesk.

### 2. Authentik

Follow [docs/AUTHENTIK.md](docs/AUTHENTIK.md) (5 minutes). You'll end up with an issuer URL, client ID and client secret, plus the groups `RMM Admins` and `RMM Technicians`.

### 3. Configure & start

```bash
git clone <this repo> hardy-rmm && cd hardy-rmm
cp .env.example .env
nano .env          # PUBLIC_URL, POSTGRES_PASSWORD, OIDC_*, RUSTDESK_HOST
docker compose up -d --build
```

The first build cross-compiles the agent for all 7 platforms, so it takes a few minutes.

### 4. Reverse proxy

The app listens on `127.0.0.1:8080`. Put your existing proxy in front of it — **WebSocket upgrades must be allowed** (`/api/agent/ws`, `/api/agent/desktop`, `/api/ws/*`).

* **Nginx Proxy Manager:** new proxy host → `http://<host>:8080`, enable *Websockets Support*, request an SSL cert.
* **Traefik:** a normal HTTP router works; Traefik proxies WebSockets automatically.
* **No proxy yet?** `docker compose --profile caddy up -d` runs Caddy with automatic Let's Encrypt for `HARDY_DOMAIN`.

Then open `https://remote.hardyvpn.online` and click **Sign in with Authentik**.

### 5. Add devices

**Clients → Deploy agent** (or **Devices → Add device**) creates an install link:

```powershell
# Windows (elevated PowerShell)
irm https://remote.hardyvpn.online/install/<token>/windows.ps1 | iex
```
```bash
# macOS
curl -fsSL https://remote.hardyvpn.online/install/<token>/macos.sh | sudo sh
# Linux
curl -fsSL https://remote.hardyvpn.online/install/<token>/linux.sh | sudo sh
```

The agent installs as a service (`Hardy RMM Agent` on Windows, systemd/SysV/OpenRC on Linux, a LaunchDaemon on macOS) and appears on the dashboard within seconds.

Manual install: download `/download/agent/<os>/<arch>` and run `hardy-agent install --server https://remote.hardyvpn.online --token <token>`. Uninstall with `hardy-agent uninstall`, or delete the device in the dashboard (the agent removes itself).

## Remote control

Click **Remote control** on a device. A pop-up viewer opens and the agent launches a capture helper inside the user's session.

| OS | How it works | Requirements |
|---|---|---|
| **Windows 10/11, Server 2016+** | Helper runs as SYSTEM in the active console (or RDP) session and follows the input desktop, so the **lock screen, sign-in screen and UAC prompts** are visible and controllable. GDI capture, `SendInput` with scan codes. Ctrl+Alt+Del via `SendSAS` (the installer enables the `SoftwareSASGeneration` policy). | none |
| **macOS 12+** | Helper runs in the console user's GUI session via `launchctl asuser`, CoreGraphics capture and `CGEvent` input (via purego, no cgo). | Grant **Screen Recording** and **Accessibility** to `/usr/local/hardy-agent/hardy-agent` in System Settings → Privacy & Security, or push a PPPC profile with your MDM. Because the binary is unsigned, re-approve after an agent update (or sign it with your Developer ID). |
| **Linux** | Finds the running Xorg server and its auth cookie, captures with X11 `GetImage`, injects input with XTEST. Works on the login screen too. | An **Xorg** session. Wayland isn't supported by the built-in viewer — pick "Ubuntu on Xorg" at login, set `WaylandEnable=false` in `/etc/gdm3/custom.conf`, or use RustDesk. |

Viewer features: multi-monitor switching, quality presets (bandwidth-adaptive with flow control), fit / 1:1 scaling, paste-as-keystrokes, Ctrl+Alt+Del, full screen, live fps/bitrate. Every session is written to the audit log.

### RustDesk (backup)

`docker compose` also runs `hbbs`/`hbbr`. On a device page, **RustDesk → Install & configure** installs the RustDesk client on the endpoint (if needed), points it at your server with its public key, and sets a random per-device password. **Connect with RustDesk** then opens your local RustDesk app straight into the session. Your technicians' RustDesk clients need the ID server and key shown under **Settings → General**.

## Roles

Roles are synced from Authentik groups at every sign-in (see `OIDC_*_GROUPS`).

| Role | Can |
|---|---|
| **viewer** | See dashboards, devices, alerts, tickets, scripts, history |
| **technician** | + remote control, terminal, run scripts, manage processes/services, reboot, edit scripts & schedules, work tickets, acknowledge alerts |
| **admin** | + delete devices, manage clients & install links, alert policies, settings, users, audit log |

`LOCAL_ADMIN_EMAIL` / `LOCAL_ADMIN_PASSWORD` enable a break-glass admin login for when Authentik is unavailable. Leave the password empty to disable it.

## Configuration reference

All settings are environment variables (see `.env.example`):

| Variable | Default | Purpose |
|---|---|---|
| `PUBLIC_URL` | – | External URL; used for OIDC redirects and agent downloads |
| `DATABASE_URL` | set by compose | Postgres DSN |
| `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` | – | Authentik provider |
| `OIDC_ADMIN_GROUPS` / `OIDC_TECH_GROUPS` / `OIDC_VIEWER_GROUPS` | `RMM Admins` / `RMM Technicians` / – | Group → role mapping |
| `OIDC_DEFAULT_ROLE` | empty (deny) | Role for users in none of the groups |
| `OIDC_SCOPES` | `openid,profile,email` | |
| `LOCAL_ADMIN_EMAIL`, `LOCAL_ADMIN_PASSWORD` | – | Break-glass account |
| `RUSTDESK_HOST`, `RUSTDESK_RELAY`, `RUSTDESK_KEY` | –, host, auto | RustDesk server details (key read from the hbbs volume) |
| `METRICS_RETENTION_DAYS` | 14 | History kept for charts |
| `SESSION_HOURS` | 12 | Dashboard session lifetime |
| `TRUST_PROXY` | true | Use `X-Forwarded-For` / `CF-Connecting-IP` for audit IPs |
| `COMPANY_NAME` | Hardy RMM | Shown in the UI; also the first client's name |

## Operations

* **Backups:** back up the `db` volume (`docker compose exec db pg_dump -U hardy hardy > backup.sql`) and the `rustdesk` volume (contains the RustDesk key pair).
* **Upgrades:** `git pull && docker compose up -d --build`. Agents check every 6 hours and self-update to the binary the server ships (or use **⋯ → Update agent**).
* **Logs:** `docker compose logs -f hardy`.
* **Health:** `GET /healthz`.

## Development

```bash
# backend
export DATABASE_URL=postgres://hardy:hardy@localhost:5432/hardy?sslmode=disable
export PUBLIC_URL=http://localhost:8080 LOCAL_ADMIN_PASSWORD=dev WEB_DIR=web/dist AGENT_DIR=dist/agents
make agents server && ./dist/hardy-server

# dashboard (Preact + TypeScript, bundled with esbuild)
cd web && npm install && npm run watch   # or npm run build
```

`web/dist` is committed so the Docker build doesn't need Node unless you change the UI (delete `web/dist` to force a rebuild in Docker).

Project layout:

```
cmd/hardy-server      server entry point
cmd/hardy-agent       agent entry point (install / service / desktop helper)
internal/server       API, OIDC, agent hub, alert engine, scheduler, installers, schema.sql
internal/agent        agent core, inventory/metrics, scripts, PTY, services, RustDesk, self-update
internal/agent/desktop  remote desktop: capture + input per OS, tile encoder
internal/proto        wire protocol shared by server and agent
web/                  dashboard
```

## Security notes

* Agents authenticate with a per-device secret (only its SHA-256 is stored); enrollment tokens can expire, be single-use and be revoked.
* All mutating API calls require a session cookie **and** a custom CSRF header; browser WebSockets check `Origin`.
* Scripts and terminals run as SYSTEM/root — only give the technician role to people you trust with that.
* Binaries are unsigned. Windows SmartScreen won't interfere with the PowerShell installer, but you may want to code-sign the agent for Defender/EDR allow-listing.

## Roadmap ideas

Patch management (Windows Update / apt / softwareupdate), Wayland capture via PipeWire, file transfer in the viewer, customer portal & email-to-ticket, SLA timers, reporting.
