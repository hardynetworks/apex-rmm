package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hardynetworks/apex-rmm/internal/proto"
)

const deviceCols = `d.id, d.client_id, d.site_id, c.name AS client_name, st.name AS site_name, d.hostname, d.display_name,
	d.os, d.platform, d.os_version, d.kernel_version, d.arch, d.cpu_model, d.cpu_cores, d.ram_total, d.manufacturer, d.model, d.serial,
	d.agent_version, d.public_ip, d.local_ips, d.logged_in_users, d.online, d.last_seen, d.boot_time, d.cpu_pct, d.mem_pct, d.disk_pct,
	d.rustdesk_id, (d.rustdesk_password <> '') AS rustdesk_ready, d.notes, d.tags, d.maintenance_until, d.created_at,
	(SELECT count(*) FROM alerts a WHERE a.device_id=d.id AND a.status<>'resolved') AS open_alerts`

const deviceFrom = ` FROM devices d LEFT JOIN clients c ON c.id=d.client_id LEFT JOIN sites st ON st.id=d.site_id `

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	where := []string{"true"}
	args := []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	if v := q.Get("client"); v != "" {
		add("d.client_id = ?", v)
	}
	if v := q.Get("site"); v != "" {
		add("d.site_id = ?", v)
	}
	if v := q.Get("os"); v != "" {
		add("d.os = ?", v)
	}
	switch q.Get("status") {
	case "online":
		where = append(where, "d.online")
	case "offline":
		where = append(where, "NOT d.online")
	}
	if v := strings.TrimSpace(q.Get("q")); v != "" {
		add("(d.hostname ILIKE ? OR d.display_name ILIKE ? OR d.platform ILIKE ? OR d.public_ip ILIKE ? OR array_to_string(d.local_ips,' ') ILIKE ? OR array_to_string(d.logged_in_users,' ') ILIKE ? OR array_to_string(d.tags,' ') ILIKE ? OR c.name ILIKE ?)", "%"+v+"%")
	}
	rows, err := s.db.Maps(r.Context(), `SELECT `+deviceCols+deviceFrom+` WHERE `+strings.Join(where, " AND ")+` ORDER BY d.online DESC, lower(coalesce(nullif(d.display_name,''), d.hostname))`, args...)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) getDevice(w http.ResponseWriter, r *http.Request) {
	d, err := s.db.Map(r.Context(), `SELECT `+deviceCols+`, d.inventory`+deviceFrom+` WHERE d.id=$1`, chi.URLParam(r, "id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, d)
}

func (s *Server) updateDevice(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DisplayName      *string    `json:"display_name"`
		Notes            *string    `json:"notes"`
		Tags             []string   `json:"tags"`
		ClientID         *string    `json:"client_id"`
		SiteID           *string    `json:"site_id"`
		MaintenanceUntil *time.Time `json:"maintenance_until"`
		ClearMaintenance bool       `json:"clear_maintenance"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	id := chi.URLParam(r, "id")
	ctx := r.Context()
	if in.DisplayName != nil {
		_, _ = s.db.Exec(ctx, `UPDATE devices SET display_name=$2 WHERE id=$1`, id, strings.TrimSpace(*in.DisplayName))
	}
	if in.Notes != nil {
		_, _ = s.db.Exec(ctx, `UPDATE devices SET notes=$2 WHERE id=$1`, id, *in.Notes)
	}
	if in.Tags != nil {
		_, _ = s.db.Exec(ctx, `UPDATE devices SET tags=$2 WHERE id=$1`, id, in.Tags)
	}
	if in.ClientID != nil {
		if _, err := s.db.Exec(ctx, `UPDATE devices SET client_id=$2, site_id=NULL WHERE id=$1`, id, *in.ClientID); err != nil {
			fail(w, badRequest("unknown client"))
			return
		}
	}
	if in.SiteID != nil {
		var site *string
		if *in.SiteID != "" {
			site = in.SiteID
		}
		_, _ = s.db.Exec(ctx, `UPDATE devices SET site_id=$2 WHERE id=$1`, id, site)
	}
	if in.MaintenanceUntil != nil {
		_, _ = s.db.Exec(ctx, `UPDATE devices SET maintenance_until=$2 WHERE id=$1`, id, *in.MaintenanceUntil)
	}
	if in.ClearMaintenance {
		_, _ = s.db.Exec(ctx, `UPDATE devices SET maintenance_until=NULL WHERE id=$1`, id)
	}
	s.audit(ctx, userFrom(r), r, "device.updated", "device", id, nil)
	s.getDevice(w, r)
}

func (s *Server) deleteDevice(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	_ = s.hub.Send(id, proto.TypeUninstall, nil)
	var host string
	err := s.db.QueryRow(r.Context(), `DELETE FROM devices WHERE id=$1 RETURNING hostname`, id).Scan(&host)
	if err != nil {
		fail(w, ErrNotFound)
		return
	}
	s.audit(r.Context(), userFrom(r), r, "device.deleted", "device", id, map[string]any{"hostname": host})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) deviceMetrics(w http.ResponseWriter, r *http.Request) {
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 || hours > 24*30 {
		hours = 24
	}
	// bucket so charts get at most ~300 points
	bucket := hours * 3600 / 300
	if bucket < 30 {
		bucket = 30
	}
	rows, err := s.db.Maps(r.Context(), `
		SELECT to_timestamp(floor(extract(epoch FROM ts)/$3)*$3) AS ts,
		       round(avg(cpu)::numeric,1)::float8 AS cpu, round(avg(mem)::numeric,1)::float8 AS mem, round(avg(disk)::numeric,1)::float8 AS disk,
		       avg(net_rx)::float8 AS net_rx, avg(net_tx)::float8 AS net_tx
		FROM device_metrics WHERE device_id=$1 AND ts > now() - make_interval(hours => $2)
		GROUP BY 1 ORDER BY 1`, chi.URLParam(r, "id"), hours, bucket)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) deviceJobs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Maps(r.Context(), `SELECT r.id, r.job_id, j.name, j.shell, r.status, r.exit_code, left(r.stdout, 20000) AS stdout, left(r.stderr, 20000) AS stderr,
		r.created_at, r.started_at, r.finished_at, j.created_by
		FROM job_results r JOIN jobs j ON j.id=r.job_id WHERE r.device_id=$1 ORDER BY r.created_at DESC LIMIT 100`, chi.URLParam(r, "id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) deviceAlerts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Maps(r.Context(), `SELECT * FROM alerts WHERE device_id=$1 ORDER BY created_at DESC LIMIT 100`, chi.URLParam(r, "id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}

// agentCall proxies a request to the agent and writes its raw JSON reply.
func (s *Server) agentCall(w http.ResponseWriter, r *http.Request, typ string, payload any, timeout time.Duration) (json.RawMessage, bool) {
	res, err := s.hub.Request(r.Context(), chi.URLParam(r, "id"), typ, payload, timeout)
	if err != nil {
		if err == ErrOffline {
			writeErr(w, 409, "device is offline")
		} else {
			fail(w, err)
		}
		return nil, false
	}
	return res, true
}

func writeRaw(w http.ResponseWriter, raw json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("[]")
	}
	_, _ = w.Write(raw)
}

func (s *Server) deviceProcesses(w http.ResponseWriter, r *http.Request) {
	if raw, ok := s.agentCall(w, r, proto.TypeListProcesses, nil, 30*time.Second); ok {
		writeRaw(w, raw)
	}
}

func (s *Server) killProcess(w http.ResponseWriter, r *http.Request) {
	pid, _ := strconv.Atoi(chi.URLParam(r, "pid"))
	if _, ok := s.agentCall(w, r, proto.TypeKillProcess, proto.KillProcess{PID: int32(pid)}, 20*time.Second); ok {
		s.audit(r.Context(), userFrom(r), r, "device.process_killed", "device", chi.URLParam(r, "id"), map[string]any{"pid": pid})
		writeJSON(w, 200, map[string]bool{"ok": true})
	}
}

func (s *Server) deviceServices(w http.ResponseWriter, r *http.Request) {
	if raw, ok := s.agentCall(w, r, proto.TypeListServices, nil, 45*time.Second); ok {
		writeRaw(w, raw)
	}
}

func (s *Server) serviceAction(w http.ResponseWriter, r *http.Request) {
	a := proto.ServiceAction{Name: chi.URLParam(r, "name"), Action: chi.URLParam(r, "action")}
	if a.Action != "start" && a.Action != "stop" && a.Action != "restart" {
		fail(w, badRequest("action must be start, stop or restart"))
		return
	}
	if _, ok := s.agentCall(w, r, proto.TypeServiceAction, a, 60*time.Second); ok {
		s.audit(r.Context(), userFrom(r), r, "device.service_"+a.Action, "device", chi.URLParam(r, "id"), map[string]any{"service": a.Name})
		writeJSON(w, 200, map[string]bool{"ok": true})
	}
}

func (s *Server) deviceSoftware(w http.ResponseWriter, r *http.Request) {
	if raw, ok := s.agentCall(w, r, proto.TypeListSoftware, nil, 90*time.Second); ok {
		writeRaw(w, raw)
	}
}

func (s *Server) devicePower(w http.ResponseWriter, r *http.Request) {
	var p proto.Power
	if err := decode(r, &p); err != nil {
		fail(w, err)
		return
	}
	if p.Action != "reboot" && p.Action != "shutdown" {
		fail(w, badRequest("action must be reboot or shutdown"))
		return
	}
	if _, ok := s.agentCall(w, r, proto.TypePower, p, 20*time.Second); ok {
		s.audit(r.Context(), userFrom(r), r, "device."+p.Action, "device", chi.URLParam(r, "id"), nil)
		writeJSON(w, 200, map[string]bool{"ok": true})
	}
}

func (s *Server) deviceRefresh(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.agentCall(w, r, proto.TypeRefreshInventory, nil, 60*time.Second); ok {
		s.getDevice(w, r)
	}
}

func (s *Server) deviceUpdateAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var goos, arch string
	if err := s.db.QueryRow(r.Context(), `SELECT os, arch FROM devices WHERE id=$1`, id).Scan(&goos, &arch); err != nil {
		fail(w, ErrNotFound)
		return
	}
	bi, err := s.agentBinary(goos, arch)
	if err != nil {
		fail(w, badRequest("no agent build for "+goos+"/"+arch))
		return
	}
	payload := proto.UpdateAgent{URL: s.conf().PublicURL + "/download/agent/" + goos + "/" + arch, SHA256: bi.sha}
	if _, ok := s.agentCall(w, r, proto.TypeUpdateAgent, payload, 120*time.Second); ok {
		s.audit(r.Context(), userFrom(r), r, "device.agent_update", "device", id, nil)
		writeJSON(w, 200, map[string]bool{"ok": true})
	}
}

// ---- remote control ----

func (s *Server) startDesktop(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	u := userFrom(r)
	if !s.hub.Online(id) {
		writeErr(w, 409, "device is offline")
		return
	}
	d, err := s.hub.newDesktop(r.Context(), id, u.ID)
	if err != nil {
		fail(w, err)
		return
	}
	s.audit(r.Context(), u, r, "device.remote_desktop", "device", id, nil)
	writeJSON(w, 200, map[string]string{"session_id": d.id})
}

func (s *Server) wsDesktop(w http.ResponseWriter, r *http.Request) {
	d := s.hub.desktop(chi.URLParam(r, "sid"))
	u := userFrom(r)
	if d == nil || d.userID != u.ID {
		writeErr(w, 404, "desktop session not found")
		return
	}
	ws, err := s.upgradeBrowser(w, r)
	if err != nil {
		return
	}
	s.hub.runDesktopViewer(d, ws)
}

func (s *Server) wsTerminal(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var goos string
	if err := s.db.QueryRow(r.Context(), `SELECT os FROM devices WHERE id=$1`, id).Scan(&goos); err != nil {
		fail(w, ErrNotFound)
		return
	}
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 32
	}
	shell := shellFor(goos, r.URL.Query().Get("shell"))
	ws, err := s.upgradeBrowser(w, r)
	if err != nil {
		return
	}
	s.audit(context.Background(), userFrom(r), r, "device.terminal", "device", id, map[string]any{"shell": shell})
	s.hub.runTerminal(id, ws, shell, cols, rows)
}

// ---- RustDesk backup remote access ----

// rustDeskConfigString builds the string accepted by `rustdesk --config`.
func rustDeskConfigString(host, relay, key string) string {
	b, _ := json.Marshal(map[string]string{"host": host, "relay": relay, "api": "", "key": key})
	enc := base64.RawURLEncoding.EncodeToString(b)
	rs := []rune(enc)
	for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
		rs[i], rs[j] = rs[j], rs[i]
	}
	return string(rs)
}

func (s *Server) provisionRustDesk(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if s.conf().RustDeskHost == "" {
		fail(w, badRequest("RustDesk is not configured on the server (set RUSTDESK_HOST)"))
		return
	}
	key := s.conf().RustDeskPublicKey()
	if key == "" {
		fail(w, badRequest("RustDesk server key not found (set RUSTDESK_KEY or mount hbbs data at RUSTDESK_KEY_FILE)"))
		return
	}
	var pw string
	_ = s.db.QueryRow(r.Context(), `SELECT rustdesk_password FROM devices WHERE id=$1`, id).Scan(&pw)
	if pw == "" {
		pw = randPassword(16)
	}
	p := proto.RustDeskProvision{IDServer: s.conf().RustDeskHost, RelayServer: s.conf().RustDeskRelay, Key: key, Password: pw,
		ConfigB64: rustDeskConfigString(s.conf().RustDeskHost, s.conf().RustDeskRelay, key)}
	raw, ok := s.agentCall(w, r, proto.TypeRustDesk, p, 10*time.Minute)
	if !ok {
		return
	}
	var res proto.RustDeskResult
	_ = json.Unmarshal(raw, &res)
	_, _ = s.db.Exec(r.Context(), `UPDATE devices SET rustdesk_password=$2, rustdesk_id=CASE WHEN $3<>'' THEN $3 ELSE rustdesk_id END WHERE id=$1`, id, pw, res.ID)
	s.audit(r.Context(), userFrom(r), r, "device.rustdesk_provisioned", "device", id, map[string]any{"rustdesk_id": res.ID})
	writeJSON(w, 200, res)
}

func (s *Server) getRustDesk(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var rid, pw string
	if err := s.db.QueryRow(r.Context(), `SELECT rustdesk_id, rustdesk_password FROM devices WHERE id=$1`, id).Scan(&rid, &pw); err != nil {
		fail(w, ErrNotFound)
		return
	}
	s.audit(r.Context(), userFrom(r), r, "device.rustdesk_credentials_viewed", "device", id, nil)
	writeJSON(w, 200, map[string]any{"id": rid, "password": pw, "server": s.conf().RustDeskHost, "key": s.conf().RustDeskPublicKey(),
		"uri": func() string {
			if rid == "" {
				return ""
			}
			return "rustdesk://connection/new/" + rid + "?password=" + pw
		}()})
}
