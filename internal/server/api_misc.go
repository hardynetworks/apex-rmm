package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// ---- dashboard ----

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := map[string]any{}
	counts, err := s.db.Map(ctx, `SELECT
		count(*) AS total,
		count(*) FILTER (WHERE online) AS online,
		count(*) FILTER (WHERE NOT online) AS offline,
		count(*) FILTER (WHERE os='windows') AS windows,
		count(*) FILTER (WHERE os='darwin') AS mac,
		count(*) FILTER (WHERE os='linux') AS linux,
		count(*) FILTER (WHERE online AND (cpu_pct > 90 OR mem_pct > 90 OR disk_pct > 90)) AS stressed
		FROM devices`)
	if err != nil {
		fail(w, err)
		return
	}
	out["devices"] = counts
	out["alerts"], _ = s.db.Map(ctx, `SELECT count(*) FILTER (WHERE status<>'resolved') AS open,
		count(*) FILTER (WHERE status<>'resolved' AND severity='critical') AS critical,
		count(*) FILTER (WHERE status<>'resolved' AND severity='warning') AS warning FROM alerts`)
	out["tickets"], _ = s.db.Map(ctx, `SELECT count(*) FILTER (WHERE status NOT IN ('resolved','closed')) AS open,
		count(*) FILTER (WHERE status NOT IN ('resolved','closed') AND assignee_id IS NULL) AS unassigned,
		count(*) FILTER (WHERE status NOT IN ('resolved','closed') AND priority IN ('high','urgent')) AS high,
		count(*) FILTER (WHERE status NOT IN ('resolved','closed') AND assignee_id=$1) AS mine,
		count(*) FILTER (WHERE resolved_at > now() - interval '7 days') AS resolved_week FROM tickets`, userFrom(r).ID)
	out["jobs"], _ = s.db.Map(ctx, `SELECT count(*) FILTER (WHERE status='success') AS success, count(*) FILTER (WHERE status IN ('failed','timeout','error')) AS failed,
		count(*) FILTER (WHERE status IN ('pending','running')) AS active FROM job_results WHERE created_at > now() - interval '24 hours'`)
	out["recent_alerts"], _ = s.db.Maps(ctx, `SELECT a.id, a.title, a.severity, a.status, a.created_at, a.device_id, coalesce(nullif(d.display_name,''), d.hostname) AS device_name
		FROM alerts a LEFT JOIN devices d ON d.id=a.device_id WHERE a.status<>'resolved' ORDER BY a.created_at DESC LIMIT 8`)
	out["recent_tickets"], _ = s.db.Maps(ctx, `SELECT t.id, t.number, t.title, t.status, t.priority, t.updated_at, c.name AS client_name
		FROM tickets t LEFT JOIN clients c ON c.id=t.client_id WHERE t.status NOT IN ('resolved','closed') ORDER BY
		CASE t.priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END, t.updated_at DESC LIMIT 8`)
	out["top_cpu"], _ = s.db.Maps(ctx, `SELECT id, coalesce(nullif(display_name,''), hostname) AS name, os, cpu_pct, mem_pct, disk_pct FROM devices WHERE online ORDER BY greatest(cpu_pct, mem_pct, disk_pct) DESC LIMIT 6`)
	out["by_client"], _ = s.db.Maps(ctx, `SELECT c.id, c.name, count(d.id) AS devices, count(d.id) FILTER (WHERE d.online) AS online FROM clients c LEFT JOIN devices d ON d.client_id=c.id GROUP BY c.id ORDER BY c.name`)
	writeJSON(w, 200, out)
}

// ---- clients & sites ----

func (s *Server) listClients(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Maps(r.Context(), `SELECT c.*,
		(SELECT count(*) FROM devices d WHERE d.client_id=c.id) AS device_count,
		(SELECT count(*) FROM devices d WHERE d.client_id=c.id AND d.online) AS online_count,
		(SELECT count(*) FROM tickets t WHERE t.client_id=c.id AND t.status NOT IN ('resolved','closed')) AS open_tickets,
		coalesce((SELECT json_agg(json_build_object('id', s.id, 'name', s.name) ORDER BY s.name) FROM sites s WHERE s.client_id=c.id), '[]') AS sites
		FROM clients c ORDER BY lower(c.name)`)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) saveClient(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name         string `json:"name"`
		Notes        string `json:"notes"`
		ContactName  string `json:"contact_name"`
		ContactEmail string `json:"contact_email"`
		ContactPhone string `json:"contact_phone"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		fail(w, badRequest("name is required"))
		return
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	var err error
	if id == "" {
		err = s.db.QueryRow(ctx, `INSERT INTO clients (name, notes, contact_name, contact_email, contact_phone) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
			in.Name, in.Notes, in.ContactName, in.ContactEmail, in.ContactPhone).Scan(&id)
		if err == nil {
			_, _ = s.db.Exec(ctx, `INSERT INTO sites (client_id, name) VALUES ($1, 'Main')`, id)
		}
	} else {
		_, err = s.db.Exec(ctx, `UPDATE clients SET name=$2, notes=$3, contact_name=$4, contact_email=$5, contact_phone=$6 WHERE id=$1`,
			id, in.Name, in.Notes, in.ContactName, in.ContactEmail, in.ContactPhone)
	}
	if err != nil {
		if strings.Contains(err.Error(), "unique") {
			fail(w, conflict("a client with that name already exists"))
			return
		}
		fail(w, err)
		return
	}
	s.audit(ctx, userFrom(r), r, "client.saved", "client", id, map[string]any{"name": in.Name})
	row, err := s.db.Map(ctx, `SELECT * FROM clients WHERE id=$1`, id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, row)
}

func (s *Server) deleteClient(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var n int
	_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM devices WHERE client_id=$1`, id).Scan(&n)
	if n > 0 {
		fail(w, conflict("move or delete this client's devices first"))
		return
	}
	if _, err := s.db.Exec(r.Context(), `DELETE FROM clients WHERE id=$1`, id); err != nil {
		fail(w, err)
		return
	}
	s.audit(r.Context(), userFrom(r), r, "client.deleted", "client", id, nil)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) addSite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		fail(w, badRequest("name is required"))
		return
	}
	row, err := s.db.Map(r.Context(), `INSERT INTO sites (client_id, name) VALUES ($1,$2) RETURNING *`, chi.URLParam(r, "id"), strings.TrimSpace(in.Name))
	if err != nil {
		fail(w, conflict("site already exists"))
		return
	}
	writeJSON(w, 200, row)
}

func (s *Server) deleteSite(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.Exec(r.Context(), `DELETE FROM sites WHERE id=$1`, chi.URLParam(r, "id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- enrollment tokens ----

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Maps(r.Context(), `SELECT t.*, c.name AS client_name, st.name AS site_name,
		(NOT t.revoked AND (t.expires_at IS NULL OR t.expires_at > now()) AND (t.max_uses=0 OR t.uses<t.max_uses)) AS active
		FROM enrollment_tokens t JOIN clients c ON c.id=t.client_id LEFT JOIN sites st ON st.id=t.site_id ORDER BY t.created_at DESC`)
	if err != nil {
		fail(w, err)
		return
	}
	for _, row := range rows {
		row["urls"] = s.installURLs(row["token"].(string))
	}
	writeJSON(w, 200, rows)
}

func (s *Server) installURLs(token string) map[string]string {
	base := s.cfg.PublicURL + "/install/" + token
	return map[string]string{
		"windows": `irm ` + base + `/windows.ps1 | iex`,
		"linux":   `curl -fsSL ` + base + `/linux.sh | sudo sh`,
		"macos":   `curl -fsSL ` + base + `/macos.sh | sudo sh`,
	}
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ClientID     string  `json:"client_id"`
		SiteID       *string `json:"site_id"`
		Description  string  `json:"description"`
		ExpiresHours int     `json:"expires_hours"`
		MaxUses      int     `json:"max_uses"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	if in.ClientID == "" {
		fail(w, badRequest("client_id is required"))
		return
	}
	if in.SiteID != nil && *in.SiteID == "" {
		in.SiteID = nil
	}
	var exp *time.Time
	if in.ExpiresHours > 0 {
		t := time.Now().Add(time.Duration(in.ExpiresHours) * time.Hour)
		exp = &t
	}
	u := userFrom(r)
	row, err := s.db.Map(r.Context(), `INSERT INTO enrollment_tokens (token, client_id, site_id, description, expires_at, max_uses, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING *`, randToken(24), in.ClientID, in.SiteID, in.Description, exp, in.MaxUses, u.Display())
	if err != nil {
		fail(w, badRequest("could not create token: "+err.Error()))
		return
	}
	row["urls"] = s.installURLs(row["token"].(string))
	s.audit(r.Context(), u, r, "enrollment_token.created", "client", in.ClientID, nil)
	writeJSON(w, 200, row)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	_, err := s.db.Exec(r.Context(), `UPDATE enrollment_tokens SET revoked=true WHERE id=$1`, chi.URLParam(r, "id"))
	if err != nil {
		fail(w, err)
		return
	}
	s.audit(r.Context(), userFrom(r), r, "enrollment_token.revoked", "token", chi.URLParam(r, "id"), nil)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- users ----

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Maps(r.Context(), `SELECT id, email, name, username, role, groups, disabled, (subject IS NULL) AS local, created_at, last_login FROM users ORDER BY lower(coalesce(nullif(name,''), email))`)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Disabled *bool `json:"disabled"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	id := chi.URLParam(r, "id")
	if id == userFrom(r).ID {
		fail(w, badRequest("you cannot disable yourself"))
		return
	}
	if in.Disabled != nil {
		_, _ = s.db.Exec(r.Context(), `UPDATE users SET disabled=$2 WHERE id=$1`, id, *in.Disabled)
		if *in.Disabled {
			_, _ = s.db.Exec(r.Context(), `DELETE FROM sessions WHERE user_id=$1`, id)
		}
	}
	s.audit(r.Context(), userFrom(r), r, "user.updated", "user", id, map[string]any{"disabled": in.Disabled})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- settings ----

type Settings struct {
	WebhookURL      string `json:"webhook_url"`
	WebhookFormat   string `json:"webhook_format"` // generic | discord | slack | ntfy
	AlertTicketUser string `json:"alert_ticket_assignee"`
}

func (s *Server) settings(r *http.Request) Settings {
	var st Settings
	s.db.Setting(r.Context(), "general", &st)
	if st.WebhookFormat == "" {
		st.WebhookFormat = "generic"
	}
	return st
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"settings": s.settings(r),
		"server": map[string]any{
			"public_url":      s.cfg.PublicURL,
			"sso_enabled":     s.cfg.OIDCEnabled(),
			"oidc_issuer":     s.cfg.OIDCIssuer,
			"admin_groups":    nz(s.cfg.OIDCAdminGroups),
			"tech_groups":     nz(s.cfg.OIDCTechGroups),
			"viewer_groups":   nz(s.cfg.OIDCViewerGroups),
			"default_role":    s.cfg.OIDCDefaultRole,
			"rustdesk_host":   s.cfg.RustDeskHost,
			"rustdesk_key":    s.cfg.RustDeskPublicKey(),
			"metrics_days":    s.cfg.MetricsRetentionDays,
			"local_admin":     s.cfg.LocalAdminPassword != "",
		},
	})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var st Settings
	if err := decode(r, &st); err != nil {
		fail(w, err)
		return
	}
	if err := s.db.SetSetting(r.Context(), "general", st); err != nil {
		fail(w, err)
		return
	}
	s.audit(r.Context(), userFrom(r), r, "settings.updated", "settings", "general", nil)
	writeJSON(w, 200, st)
}

func (s *Server) testWebhook(w http.ResponseWriter, r *http.Request) {
	st := s.settings(r)
	if st.WebhookURL == "" {
		fail(w, badRequest("no webhook URL configured"))
		return
	}
	if err := s.notify(r.Context(), st, "info", "Hardy RMM test notification", "If you can read this, alert notifications are working."); err != nil {
		fail(w, badRequest("webhook failed: "+err.Error()))
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- audit ----

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	args := []any{limit}
	where := "true"
	if q := r.URL.Query().Get("q"); q != "" {
		args = append(args, "%"+q+"%")
		where = "(action ILIKE $2 OR user_name ILIKE $2 OR target_id ILIKE $2 OR details::text ILIKE $2)"
	}
	rows, err := s.db.Maps(r.Context(), `SELECT * FROM audit_log WHERE `+where+` ORDER BY id DESC LIMIT $1`, args...)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}
