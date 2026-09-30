# Apex RMM

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

---

## Architecture

```
 Browser ──HTTPS/WSS──┐                         ┌── hbbs / hbbr (RustDesk server, ports 21115-21119)
                      ▼                         │
              ┌──────────────────┐              │
              │   apex-server   │  Postgres    │
              │  Go API + React  │◄──────────►  db
              │  dashboard + hub │
              └──────────────────┘
                 ▲            ▲
    WSS control  │            │  WSS screen stream (per session)
                 │            │
        ┌────────┴───┐   ┌────┴──────────────┐
        │ apex-agent│──►│ apex-agent       │   spawned in the logged-in user's
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
| `rmm.example.com` | your Docker host | Can be Cloudflare-proxied (WebSockets work) |
| `rustdesk.example.com` | your Docker host | **DNS only** (grey cloud) – RustDesk uses raw TCP/UDP |

Open/forward TCP **21115-21119** and UDP **21116** to the host for RustDesk.

### 2. Start it

```bash
git clone https://github.com/hardynetworks/apex-rmm.git && cd apex-rmm
docker compose up -d --build
```

No config file is needed. The first build cross-compiles the agent for all 7 platforms, so it takes a few minutes. A random database password and encryption key are generated automatically on first start.

### 3. Run the setup wizard

Open `http://<your-server>:8080`. The wizard asks for a **setup code** (proof that you own the server), which is printed in the log:

```bash
docker compose logs apex | grep "setup code"
```

It then asks for your company name and public URL and creates your first admin account (a local login that keeps working even if SSO is down).

### 4. Finish in Settings → General

Everything else is set in the dashboard, and changes apply immediately without a restart:

* **Server:** public URL and company name.
* **Single sign-on:** Authentik issuer URL, client ID and secret, and which Authentik groups map to admin, technician and viewer. There's a **Test** button for the issuer, and the redirect URI to paste into Authentik is shown for you. See [docs/AUTHENTIK.md](docs/AUTHENTIK.md).
* **RustDesk:** the host devices use to reach the bundled RustDesk server. The server key is detected automatically.
* **Notifications:** Discord, Slack, ntfy or generic webhook.

**Reverse proxy:** the dashboard is published on port 8080. Put your existing proxy in front of it; **WebSocket upgrades must be allowed** (`/api/agent/ws`, `/api/agent/desktop`, `/api/ws/*`).

* **Nginx Proxy Manager:** new proxy host → `http://<host>:8080`, enable *Websockets Support*, request an SSL cert.
* **Traefik:** a normal HTTP router works; Traefik proxies WebSockets automatically.
* **No proxy yet?** `docker compose --profile caddy up -d` runs Caddy with automatic Let's Encrypt for `APEX_DOMAIN` (set it in `.env`).

Once HTTPS works, update **Public URL** in Settings to the `https://` address before deploying agents.

### 5. Add devices

**Clients → Deploy agent** (or **Devices → Add device**) creates an install link:

```powershell
# Windows (elevated PowerShell)
irm https://rmm.example.com/install/<token>/windows.ps1 | iex
```
```bash
# macOS
curl -fsSL https://rmm.example.com/install/<token>/macos.sh | sudo sh
# Linux
curl -fsSL https://rmm.example.com/install/<token>/linux.sh | sudo sh
```

The agent installs as a service (`Apex RMM Agent` on Windows, systemd/SysV/OpenRC on Linux, a LaunchDaemon on macOS) and appears on the dashboard within seconds.

Manual install: download `/download/agent/<os>/<arch>` and run `apex-agent install --server https://rmm.example.com --token <token>`. Uninstall with `apex-agent uninstall`, or delete the device in the dashboard (the agent removes itself).

## Remote control

Click **Remote control** on a device. A pop-up viewer opens and the agent launches a capture helper inside the user's session.

| OS | How it works | Requirements |
|---|---|---|
| **Windows 10/11, Server 2016+** | Helper runs as SYSTEM in the active console (or RDP) session and follows the input desktop, so the **lock screen, sign-in screen and UAC prompts** are visible and controllable. GDI capture, `SendInput` with scan codes. Ctrl+Alt+Del via `SendSAS` (the installer enables the `SoftwareSASGeneration` policy). | none |
| **macOS 12+** | Helper runs in the console user's GUI session via `launchctl asuser`, CoreGraphics capture and `CGEvent` input (via purego, no cgo). | Grant **Screen Recording** and **Accessibility** to `/usr/local/apex-agent/apex-agent` in System Settings → Privacy & Security, or push a PPPC profile with your MDM. Because the binary is unsigned, re-approve after an agent update (or sign it with your Developer ID). |
| **Linux** | Finds the running Xorg server and its auth cookie, captures with X11 `GetImage`, injects input with XTEST. Works on the login screen too. | An **Xorg** session. Wayland isn't supported by the built-in viewer — pick "Ubuntu on Xorg" at login, set `WaylandEnable=false` in `/etc/gdm3/custom.conf`, or use RustDesk. |

Viewer features: multi-monitor switching, quality presets (bandwidth-adaptive with flow control), fit / 1:1 scaling, paste-as-keystrokes, Ctrl+Alt+Del, full screen, live fps/bitrate. Every session is written to the audit log.

### On a phone or tablet

The viewer switches to touch controls automatically, with a bar at the bottom of the screen:

| | Trackpad mode (default) | Touch mode |
|---|---|---|
| Move the mouse | Drag one finger (moves an on-screen cursor) | Tap where you want it |
| Left click | Tap anywhere | Tap |
| Right click | Two-finger tap | Long press or two-finger tap |
| Drag | Double-tap and drag, or long press then drag | Touch and drag |
| Scroll | Two-finger drag | Two-finger drag |
| Zoom | Pinch (tap **1×** to reset) | Pinch |

* **Left / Right** buttons click at the cursor. Hold **Left** and drag with another finger to drag windows or select text.
* **Keyboard** opens the phone's keyboard; text is typed on the remote device (works with autocorrect and swipe typing).
* **Fn** opens a key row: Esc, Tab, arrows, Home/End, PgUp/PgDn, Del, F1–F12, Ctrl+Alt+Del, and sticky **Ctrl / Alt / Shift / Win** (tap Ctrl, then type `c` for Ctrl+C).
* Tap **?** in the top bar for a gesture cheat sheet. The mode you pick is remembered on that device.

### RustDesk (backup)

`docker compose` also runs `hbbs`/`hbbr`. On a device page, **RustDesk → Install & configure** installs the RustDesk client on the endpoint (if needed), points it at your server with its public key, and sets a random per-device password. **Connect with RustDesk** then opens your local RustDesk app straight into the session. Your technicians' RustDesk clients need the ID server and key shown under **Settings → General**.

## Roles

Roles are synced from Authentik groups at every sign-in (see `OIDC_*_GROUPS`).

| Role | Can |
|---|---|
| **viewer** | See dashboards, devices, alerts, tickets, scripts, history |
| **technician** | + remote control, terminal, run scripts, manage processes/services, reboot, edit scripts & schedules, work tickets, acknowledge alerts |
| **admin** | + delete devices, manage clients & install links, alert policies, settings, users, audit log |

The admin account created in the setup wizard is a local login that keeps working when Authentik is unavailable; local users can change their password under Settings → General.

## Configuration reference

Most settings live in **Settings → General** and are stored in the database (the OIDC client secret is encrypted with the generated app key). A `.env` file is optional: any variable set there overrides the dashboard value and shows as locked in the UI. See `.env.example`.

| Variable | Where | Purpose |
|---|---|---|
| `PUBLIC_URL`, `COMPANY_NAME` | Settings or env | External URL (OIDC redirects, agent downloads) and display name |
| `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` | Settings or env | Authentik provider |
| `OIDC_ADMIN_GROUPS` / `OIDC_TECH_GROUPS` / `OIDC_VIEWER_GROUPS` | Settings or env | Group → role mapping (defaults `RMM Admins` / `RMM Technicians` / –) |
| `OIDC_DEFAULT_ROLE` | Settings or env | Role for users in none of the groups (empty = deny) |
| `RUSTDESK_HOST`, `RUSTDESK_RELAY`, `RUSTDESK_KEY` | Settings or env | RustDesk server details (key auto-detected from the hbbs volume) |
| `LOCAL_ADMIN_EMAIL`, `LOCAL_ADMIN_PASSWORD` | env only | Extra break-glass admin; also skips the setup wizard |
| `APEX_BIND` | env only | Published port, default `8080` (use `127.0.0.1:8080` behind a local proxy) |
| `METRICS_RETENTION_DAYS`, `SESSION_HOURS`, `TRUST_PROXY` | env only | History kept (14), session length (12 h), trust `X-Forwarded-For` (true) |
| `DATABASE_URL` or `DB_HOST`/`DB_PASSWORD_FILE` | set by compose | Postgres connection |

## Upgrading from Hardy RMM

This project was called Hardy RMM before. The rename changed the agent's service name and install folders and the Docker volume names, so an existing install starts fresh:

1. Stop the old stack (`docker compose down` in the old folder), then clone this repo and `docker compose up -d --build` as in the quick start. The old `hardy-rmm_*` volumes are left untouched; delete them with `docker volume rm` once you don't need them.
2. Re-deploy agents with a new install link. The new installer removes the old `hardy-agent` service and folders automatically before installing `apex-agent`.

## Operations

* **Backups:** back up the `db` volume (`docker compose exec db pg_dump -U apex apex > backup.sql`), the `secrets` volume (database password and the key that decrypts stored secrets) and the `rustdesk` volume (RustDesk key pair).
* **Upgrades:** `git pull && docker compose up -d --build`. Agents check every 6 hours and self-update to the binary the server ships (or use **⋯ → Update agent**).
* **Logs:** `docker compose logs -f apex`.
* **Health:** `GET /healthz`.

## Development

```bash
# backend
export DATABASE_URL=postgres://apex:apex@localhost:5432/apex?sslmode=disable
export PUBLIC_URL=http://localhost:8080 LOCAL_ADMIN_PASSWORD=dev WEB_DIR=web/dist AGENT_DIR=dist/agents
make agents server && ./dist/apex-server

# dashboard (Preact + TypeScript, bundled with esbuild)
cd web && npm install && npm run watch   # or npm run build
```

The Docker build compiles the dashboard itself (`web/dist` isn't committed).

Project layout:

```
cmd/apex-server      server entry point
cmd/apex-agent       agent entry point (install / service / desktop helper)
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
