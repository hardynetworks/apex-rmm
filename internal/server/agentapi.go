package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/hardynetworks/hardy-rmm/internal/proto"
)

var agentUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 64 << 10,
	CheckOrigin:     func(r *http.Request) bool { return true }, // agents authenticate with bearer secrets
}

// authAgent validates "Authorization: Bearer <deviceID>.<secret>".
func (s *Server) authAgent(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return "", false
	}
	id, secret, ok := strings.Cut(strings.TrimPrefix(h, "Bearer "), ".")
	if !ok {
		return "", false
	}
	var hash string
	if err := s.db.QueryRow(r.Context(), `SELECT secret_hash FROM devices WHERE id=$1`, id).Scan(&hash); err != nil {
		return "", false
	}
	return id, constEq(hash, sha256Hex(secret))
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req proto.EnrollRequest
	if err := decode(r, &req); err != nil {
		fail(w, err)
		return
	}
	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var tokID, clientID string
	var siteID *string
	err = tx.QueryRow(ctx, `
		SELECT id, client_id, site_id FROM enrollment_tokens
		WHERE token=$1 AND NOT revoked AND (expires_at IS NULL OR expires_at > now()) AND (max_uses = 0 OR uses < max_uses)
		FOR UPDATE`, req.Token).Scan(&tokID, &clientID, &siteID)
	if err != nil {
		writeErr(w, http.StatusForbidden, "invalid or expired enrollment token")
		return
	}
	secret := randToken(32)
	var devID string
	inv := req.Inventory
	err = tx.QueryRow(ctx, `INSERT INTO devices (client_id, site_id, secret_hash, hostname, os) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		clientID, siteID, sha256Hex(secret), inv.Hostname, inv.OS).Scan(&devID)
	if err != nil {
		fail(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE enrollment_tokens SET uses=uses+1 WHERE id=$1`, tokID); err != nil {
		fail(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		fail(w, err)
		return
	}
	s.updateInventory(ctx, devID, &inv)
	s.audit(ctx, nil, r, "device.enrolled", "device", devID, map[string]any{"hostname": inv.Hostname, "os": inv.OS, "ip": s.clientIP(r)})
	writeJSON(w, 200, proto.EnrollResponse{DeviceID: devID, Secret: secret})
}

func (s *Server) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authAgent(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "bad agent credentials")
		return
	}
	ws, err := agentUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.hub.serveAgent(id, s.clientIP(r), ws)
}

// handleDesktopHelper accepts the screen-streaming helper's connection.
func (s *Server) handleDesktopHelper(w http.ResponseWriter, r *http.Request) {
	d := s.hub.desktop(r.URL.Query().Get("session"))
	if d == nil || !constEq(d.token, r.URL.Query().Get("token")) || d.agentConn != nil {
		writeErr(w, http.StatusUnauthorized, "invalid desktop session")
		return
	}
	ws, err := agentUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.hub.attachDesktopAgent(d, ws)
}

// ---- inventory & metrics ----

func (s *Server) updateInventory(ctx context.Context, id string, inv *proto.Inventory) {
	var ips []string
	for _, n := range inv.NICs {
		for _, a := range n.Addrs {
			if !strings.HasPrefix(a, "127.") && !strings.HasPrefix(a, "::1") && !strings.HasPrefix(a, "fe80") && !strings.HasPrefix(a, "169.254.") {
				ips = append(ips, strings.Split(a, "/")[0])
			}
		}
	}
	raw, _ := json.Marshal(inv)
	var boot *time.Time
	if inv.BootTime > 0 {
		t := time.Unix(inv.BootTime, 0)
		boot = &t
	}
	_, err := s.db.Exec(ctx, `UPDATE devices SET hostname=$2, os=$3, platform=$4, os_version=$5, kernel_version=$6, arch=$7,
		cpu_model=$8, cpu_cores=$9, ram_total=$10, manufacturer=$11, model=$12, serial=$13, agent_version=$14,
		local_ips=$15, logged_in_users=$16, inventory=$17, boot_time=$18, last_seen=now(),
		rustdesk_id = CASE WHEN $19 <> '' THEN $19 ELSE rustdesk_id END
		WHERE id=$1`, id, inv.Hostname, inv.OS, inv.Platform, inv.OSVersion, inv.KernelVersion, inv.Arch,
		inv.CPUModel, inv.CPUThreads, int64(inv.RAMTotal), inv.Manufacturer, inv.Model, inv.Serial, inv.AgentVersion,
		nz(ips), nz(inv.LoggedInUsers), raw, boot, inv.RustDeskID)
	if err != nil {
		slog.Error("update inventory", "device", id, "err", err)
	}
}

func (s *Server) recordMetrics(ctx context.Context, id string, m *proto.Metrics) {
	users := nz(m.Users)
	_, err := s.db.Exec(ctx, `UPDATE devices SET cpu_pct=$2, mem_pct=$3, disk_pct=$4, last_seen=now(), online=true, logged_in_users=$5,
		inventory = jsonb_set(inventory, '{disks}', $6::jsonb) WHERE id=$1`, id, m.CPU, m.Mem, m.Disk, users, mustJSON(m.Disks))
	if err != nil {
		slog.Error("update metrics", "device", id, "err", err)
	}
	_, _ = s.db.Exec(ctx, `INSERT INTO device_metrics (device_id, cpu, mem, disk, net_rx, net_tx) VALUES ($1,$2,$3,$4,$5,$6)`,
		id, m.CPU, m.Mem, m.Disk, int64(m.NetRx), int64(m.NetTx))
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	if string(b) == "null" {
		return "[]"
	}
	return string(b)
}

func (s *Server) recordScriptResult(ctx context.Context, deviceID string, r *proto.ScriptResult) {
	_, err := s.db.Exec(ctx, `UPDATE job_results SET status=$3, exit_code=$4, stdout=$5, stderr=$6, started_at=to_timestamp($7), finished_at=to_timestamp($8)
		WHERE id=$1 AND device_id=$2`, r.ResultID, deviceID, r.Status, r.ExitCode, r.Stdout, r.Stderr, float64(r.Started), float64(r.Finished))
	if err != nil {
		slog.Error("record script result", "err", err)
	}
}

// dispatchPending sends queued script runs to a device that just came online.
func (s *Server) dispatchPending(deviceID string) {
	ctx := context.Background()
	time.Sleep(2 * time.Second)
	_, _ = s.db.Exec(ctx, `UPDATE job_results SET status='expired', finished_at=now() WHERE device_id=$1 AND status='pending' AND created_at < now() - interval '24 hours'`, deviceID)
	rows, err := s.db.Maps(ctx, `SELECT r.id, j.shell, j.body, j.args, j.timeout_seconds FROM job_results r JOIN jobs j ON j.id=r.job_id
		WHERE r.device_id=$1 AND r.status='pending' ORDER BY r.created_at`, deviceID)
	if err != nil {
		return
	}
	for _, row := range rows {
		s.sendScript(ctx, deviceID, row["id"].(string), row["shell"].(string), row["body"].(string), toStrings(row["args"]), int(row["timeout_seconds"].(int32)))
	}
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	return nil
}

func (s *Server) sendScript(ctx context.Context, deviceID, resultID, shell, body string, args []string, timeout int) {
	err := s.hub.Send(deviceID, proto.TypeRunScript, proto.RunScript{ResultID: resultID, Shell: shell, Body: body, Args: args, Timeout: timeout})
	if err == nil {
		_, _ = s.db.Exec(ctx, `UPDATE job_results SET status='running', started_at=now() WHERE id=$1 AND status='pending'`, resultID)
	}
}

// ---- agent binaries & installers ----

type binInfo struct {
	path string
	sha  string
	mod  time.Time
}

var binCache sync.Map

func (s *Server) agentBinary(goos, arch string) (*binInfo, error) {
	if !validPlatform(goos, arch) {
		return nil, ErrNotFound
	}
	name := fmt.Sprintf("hardy-agent-%s-%s", goos, arch)
	if goos == "windows" {
		name += ".exe"
	}
	p := filepath.Join(s.conf().AgentDir, name)
	st, err := os.Stat(p)
	if err != nil {
		return nil, ErrNotFound
	}
	if v, ok := binCache.Load(p); ok {
		if bi := v.(*binInfo); bi.mod.Equal(st.ModTime()) {
			return bi, nil
		}
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	bi := &binInfo{path: p, sha: hex.EncodeToString(h.Sum(nil)), mod: st.ModTime()}
	binCache.Store(p, bi)
	return bi, nil
}

func validPlatform(goos, arch string) bool {
	switch goos + "/" + arch {
	case "windows/amd64", "windows/arm64", "linux/amd64", "linux/arm64", "linux/arm", "darwin/amd64", "darwin/arm64":
		return true
	}
	return false
}

func (s *Server) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	bi, err := s.agentBinary(chi.URLParam(r, "os"), chi.URLParam(r, "arch"))
	if err != nil {
		writeErr(w, 404, "agent binary not built for this platform")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Agent-SHA256", bi.sha)
	w.Header().Set("Content-Disposition", "attachment; filename="+filepath.Base(bi.path))
	http.ServeFile(w, r, bi.path)
}

func (s *Server) handleAgentVersion(w http.ResponseWriter, r *http.Request) {
	bi, err := s.agentBinary(r.URL.Query().Get("os"), r.URL.Query().Get("arch"))
	if err != nil {
		writeErr(w, 404, "no binary")
		return
	}
	writeJSON(w, 200, map[string]string{"sha256": bi.sha, "url": s.conf().PublicURL + "/download/agent/" + r.URL.Query().Get("os") + "/" + r.URL.Query().Get("arch")})
}

var installTemplates = map[string]*template.Template{
	"linux.sh": template.Must(template.New("l").Parse(`#!/bin/sh
# Hardy RMM agent installer for Linux
set -e
SERVER="{{.URL}}"
TOKEN="{{.Token}}"
if [ "$(id -u)" -ne 0 ]; then echo "Please run as root (sudo)." >&2; exit 1; fi
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7l|armv6l) ARCH=arm ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
TMP="$(mktemp /tmp/hardy-agent.XXXXXX)"
echo "Downloading Hardy RMM agent ($ARCH)..."
if command -v curl >/dev/null 2>&1; then curl -fsSL "$SERVER/download/agent/linux/$ARCH" -o "$TMP"; else wget -qO "$TMP" "$SERVER/download/agent/linux/$ARCH"; fi
chmod +x "$TMP"
"$TMP" install --server "$SERVER" --token "$TOKEN"
rm -f "$TMP"
`)),
	"macos.sh": template.Must(template.New("m").Parse(`#!/bin/sh
# Hardy RMM agent installer for macOS
set -e
SERVER="{{.URL}}"
TOKEN="{{.Token}}"
if [ "$(id -u)" -ne 0 ]; then echo "Please run with sudo." >&2; exit 1; fi
case "$(uname -m)" in
  arm64) ARCH=arm64 ;;
  *) ARCH=amd64 ;;
esac
TMP="$(mktemp /tmp/hardy-agent.XXXXXX)"
echo "Downloading Hardy RMM agent ($ARCH)..."
curl -fsSL "$SERVER/download/agent/darwin/$ARCH" -o "$TMP"
chmod +x "$TMP"
xattr -d com.apple.quarantine "$TMP" 2>/dev/null || true
"$TMP" install --server "$SERVER" --token "$TOKEN"
rm -f "$TMP"
echo ""
echo "For remote control, grant Screen Recording and Accessibility to"
echo "  /usr/local/hardy-agent/hardy-agent"
echo "in System Settings > Privacy & Security (or push a PPPC profile via MDM)."
`)),
	"windows.ps1": template.Must(template.New("w").Parse(`# Hardy RMM agent installer for Windows (run in an elevated PowerShell)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$Server = '{{.URL}}'
$Token = '{{.Token}}'
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Please run PowerShell as Administrator.' }
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$tmp = Join-Path $env:TEMP ('hardy-agent-setup-' + [guid]::NewGuid().ToString('N') + '.exe')
Write-Host "Downloading Hardy RMM agent ($arch)..."
Invoke-WebRequest -UseBasicParsing -Uri "$Server/download/agent/windows/$arch" -OutFile $tmp
& $tmp install --server $Server --token $Token
if ($LASTEXITCODE -ne 0) { throw "Agent install failed with exit code $LASTEXITCODE" }
Remove-Item $tmp -Force -ErrorAction SilentlyContinue
Write-Host 'Hardy RMM agent installed.'
`)),
}

func (s *Server) handleInstaller(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	tpl := installTemplates[chi.URLParam(r, "file")]
	if tpl == nil {
		http.NotFound(w, r)
		return
	}
	var ok bool
	_ = s.db.QueryRow(r.Context(), `SELECT true FROM enrollment_tokens WHERE token=$1 AND NOT revoked AND (expires_at IS NULL OR expires_at > now()) AND (max_uses=0 OR uses<max_uses)`, token).Scan(&ok)
	if !ok {
		http.Error(w, "# invalid or expired enrollment token\nexit 1\n", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = tpl.Execute(w, map[string]string{"URL": s.conf().PublicURL, "Token": token})
}
