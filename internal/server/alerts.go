package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// ---- policies ----

func (s *Server) listPolicies(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Maps(r.Context(), `SELECT p.*, c.name AS client_name,
		(SELECT count(*) FROM alerts a WHERE a.policy_id=p.id AND a.status<>'resolved') AS open_alerts
		FROM alert_policies p LEFT JOIN clients c ON c.id=p.client_id ORDER BY p.name`)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) savePolicy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name            string  `json:"name"`
		Enabled         bool    `json:"enabled"`
		Metric          string  `json:"metric"`
		Operator        string  `json:"operator"`
		Threshold       float64 `json:"threshold"`
		DurationMinutes int     `json:"duration_minutes"`
		Severity        string  `json:"severity"`
		ClientID        *string `json:"client_id"`
		CreateTicket    bool    `json:"create_ticket"`
		Notify          bool    `json:"notify"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	switch in.Metric {
	case "cpu", "mem", "disk", "offline":
	default:
		fail(w, badRequest("metric must be cpu, mem, disk or offline"))
		return
	}
	if in.Operator != "<" {
		in.Operator = ">"
	}
	if in.Severity != "info" && in.Severity != "critical" {
		in.Severity = "warning"
	}
	if in.DurationMinutes <= 0 {
		in.DurationMinutes = 5
	}
	if in.ClientID != nil && *in.ClientID == "" {
		in.ClientID = nil
	}
	if strings.TrimSpace(in.Name) == "" {
		fail(w, badRequest("name is required"))
		return
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	var err error
	if id == "" {
		err = s.db.QueryRow(ctx, `INSERT INTO alert_policies (name, enabled, metric, operator, threshold, duration_minutes, severity, client_id, create_ticket, notify)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`, in.Name, in.Enabled, in.Metric, in.Operator, in.Threshold, in.DurationMinutes, in.Severity, in.ClientID, in.CreateTicket, in.Notify).Scan(&id)
	} else {
		_, err = s.db.Exec(ctx, `UPDATE alert_policies SET name=$2, enabled=$3, metric=$4, operator=$5, threshold=$6, duration_minutes=$7, severity=$8, client_id=$9, create_ticket=$10, notify=$11 WHERE id=$1`,
			id, in.Name, in.Enabled, in.Metric, in.Operator, in.Threshold, in.DurationMinutes, in.Severity, in.ClientID, in.CreateTicket, in.Notify)
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.audit(ctx, userFrom(r), r, "alert_policy.saved", "alert_policy", id, map[string]any{"name": in.Name})
	row, _ := s.db.Map(ctx, `SELECT * FROM alert_policies WHERE id=$1`, id)
	writeJSON(w, 200, row)
}

func (s *Server) deletePolicy(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.Exec(r.Context(), `DELETE FROM alert_policies WHERE id=$1`, chi.URLParam(r, "id")); err != nil {
		fail(w, err)
		return
	}
	s.audit(r.Context(), userFrom(r), r, "alert_policy.deleted", "alert_policy", chi.URLParam(r, "id"), nil)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- alerts ----

func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) {
	where := "a.status<>'resolved'"
	switch r.URL.Query().Get("status") {
	case "all":
		where = "true"
	case "resolved":
		where = "a.status='resolved'"
	case "open":
		where = "a.status='open'"
	case "acknowledged":
		where = "a.status='acknowledged'"
	}
	rows, err := s.db.Maps(r.Context(), `SELECT a.*, coalesce(nullif(d.display_name,''), d.hostname) AS device_name, d.os, c.name AS client_name, t.number AS ticket_number
		FROM alerts a LEFT JOIN devices d ON d.id=a.device_id LEFT JOIN clients c ON c.id=d.client_id LEFT JOIN tickets t ON t.id=a.ticket_id
		WHERE `+where+` ORDER BY CASE a.severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END, a.created_at DESC LIMIT 500`)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) ackAlert(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	_, err := s.db.Exec(r.Context(), `UPDATE alerts SET status='acknowledged', acknowledged_by=$2, acknowledged_at=now() WHERE id=$1 AND status='open'`, chi.URLParam(r, "id"), u.Display())
	if err != nil {
		fail(w, err)
		return
	}
	s.audit(r.Context(), u, r, "alert.acknowledged", "alert", chi.URLParam(r, "id"), nil)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) resolveAlert(w http.ResponseWriter, r *http.Request) {
	_, err := s.db.Exec(r.Context(), `UPDATE alerts SET status='resolved', resolved_at=now() WHERE id=$1`, chi.URLParam(r, "id"))
	if err != nil {
		fail(w, err)
		return
	}
	s.audit(r.Context(), userFrom(r), r, "alert.resolved", "alert", chi.URLParam(r, "id"), nil)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) alertToTicket(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	u := userFrom(r)
	tid, err := s.ticketForAlert(r.Context(), id, u)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"ticket_id": tid})
}

func (s *Server) ticketForAlert(ctx context.Context, alertID string, u *User) (string, error) {
	var title, msg, sev string
	var deviceID, clientID, existing *string
	err := s.db.QueryRow(ctx, `SELECT a.title, a.message, a.severity, a.device_id, d.client_id, a.ticket_id FROM alerts a LEFT JOIN devices d ON d.id=a.device_id WHERE a.id=$1`, alertID).
		Scan(&title, &msg, &sev, &deviceID, &clientID, &existing)
	if err != nil {
		return "", ErrNotFound
	}
	if existing != nil {
		return *existing, nil
	}
	prio := map[string]string{"critical": "high", "warning": "medium", "info": "low"}[sev]
	by := "system"
	var uid *string
	if u != nil {
		by = u.Display()
		uid = &u.ID
	}
	var tid string
	err = s.db.QueryRow(ctx, `INSERT INTO tickets (title, description, priority, client_id, device_id, source, created_by) VALUES ($1,$2,$3,$4,$5,'alert',$6) RETURNING id`,
		"[Alert] "+title, msg, prio, clientID, deviceID, by).Scan(&tid)
	if err != nil {
		return "", err
	}
	_, _ = s.db.Exec(ctx, `UPDATE alerts SET ticket_id=$2 WHERE id=$1`, alertID, tid)
	_, _ = s.db.Exec(ctx, `INSERT INTO ticket_comments (ticket_id, author_id, author_name, body, internal, kind) VALUES ($1,$2,$3,$4,true,'event')`, tid, uid, by, "Ticket created from alert")
	return tid, nil
}

// ---- evaluator ----

func (s *Server) alertLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.evaluateAlerts(ctx)
	}
}

func (s *Server) evaluateAlerts(ctx context.Context) {
	policies, err := s.db.Maps(ctx, `SELECT * FROM alert_policies WHERE enabled`)
	if err != nil {
		return
	}
	for _, p := range policies {
		pid := p["id"].(string)
		metric := p["metric"].(string)
		op := p["operator"].(string)
		threshold := float64(p["threshold"].(float32))
		dur := int(p["duration_minutes"].(int32))
		var clientID any = p["client_id"]

		type hit struct {
			id, name string
			value    float64
			firing   bool
		}
		var hits []hit
		if metric == "offline" {
			rows, err := s.db.Maps(ctx, `SELECT id, coalesce(nullif(display_name,''), hostname) AS name,
				(NOT online AND last_seen < now() - make_interval(mins => $1)) AS firing,
				(extract(epoch FROM now()-coalesce(last_seen, created_at))/60)::float8 AS mins
				FROM devices WHERE ($2::text IS NULL OR client_id=$2) AND (maintenance_until IS NULL OR maintenance_until < now())`, dur, clientID)
			if err != nil {
				continue
			}
			for _, r := range rows {
				hits = append(hits, hit{r["id"].(string), r["name"].(string), toF(r["mins"]), r["firing"] == true})
			}
		} else {
			cmp := ">"
			if op == "<" {
				cmp = "<"
			}
			rows, err := s.db.Maps(ctx, `SELECT d.id, coalesce(nullif(d.display_name,''), d.hostname) AS name, avg(m.`+metric+`)::float8 AS value, count(m.*) AS n
				FROM devices d JOIN device_metrics m ON m.device_id=d.id AND m.ts > now() - make_interval(mins => $1)
				WHERE d.online AND ($2::text IS NULL OR d.client_id=$2) AND (d.maintenance_until IS NULL OR d.maintenance_until < now())
				GROUP BY d.id`, dur, clientID)
			if err != nil {
				slog.Error("alert eval", "err", err)
				continue
			}
			for _, r := range rows {
				v := toF(r["value"])
				n := r["n"].(int64)
				firing := n >= 2 && ((cmp == ">" && v > threshold) || (cmp == "<" && v < threshold))
				hits = append(hits, hit{r["id"].(string), r["name"].(string), v, firing})
			}
		}
		for _, h := range hits {
			var openID string
			_ = s.db.QueryRow(ctx, `SELECT id FROM alerts WHERE policy_id=$1 AND device_id=$2 AND status<>'resolved' LIMIT 1`, pid, h.id).Scan(&openID)
			if h.firing && openID == "" {
				s.raiseAlert(ctx, p, h.id, h.name, h.value)
			} else if !h.firing && openID != "" {
				_, _ = s.db.Exec(ctx, `UPDATE alerts SET status='resolved', resolved_at=now() WHERE id=$1`, openID)
			}
		}
	}
}

func toF(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	}
	var f float64
	fmt.Sscan(fmt.Sprint(v), &f)
	return f
}

var metricNames = map[string]string{"cpu": "CPU usage", "mem": "Memory usage", "disk": "Disk usage", "offline": "Device offline"}

func (s *Server) raiseAlert(ctx context.Context, p map[string]any, deviceID, deviceName string, value float64) {
	metric := p["metric"].(string)
	sev := p["severity"].(string)
	var title, msg string
	if metric == "offline" {
		title = fmt.Sprintf("%s is offline", deviceName)
		msg = fmt.Sprintf("%s has not checked in for %.0f minutes (policy: %s).", deviceName, value, p["name"])
	} else {
		title = fmt.Sprintf("%s: %s %.0f%%", deviceName, metricNames[metric], value)
		msg = fmt.Sprintf("%s on %s averaged %.1f%% over %d minutes (threshold %s %.0f%%, policy: %s).",
			metricNames[metric], deviceName, value, p["duration_minutes"], p["operator"], toF(p["threshold"]), p["name"])
	}
	var aid string
	if err := s.db.QueryRow(ctx, `INSERT INTO alerts (policy_id, device_id, severity, title, message, value) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		p["id"], deviceID, sev, title, msg, value).Scan(&aid); err != nil {
		slog.Error("raise alert", "err", err)
		return
	}
	slog.Info("alert raised", "title", title)
	if p["create_ticket"] == true {
		_, _ = s.ticketForAlert(ctx, aid, nil)
	}
	if p["notify"] == true {
		var st Settings
		s.db.Setting(ctx, "general", &st)
		if st.WebhookURL != "" {
			go func() {
				if err := s.notify(context.Background(), st, sev, title, msg+"\n"+s.conf().PublicURL+"/devices/"+deviceID); err != nil {
					slog.Warn("webhook notify failed", "err", err)
				}
			}()
		}
	}
}

// notify posts to the configured webhook (generic JSON, Discord, Slack or ntfy).
func (s *Server) notify(ctx context.Context, st Settings, severity, title, body string) error {
	var payload []byte
	contentType := "application/json"
	icon := map[string]string{"critical": "🔴", "warning": "🟠", "info": "🔵"}[severity]
	switch st.WebhookFormat {
	case "discord":
		payload, _ = json.Marshal(map[string]any{"username": s.conf().CompanyName, "content": fmt.Sprintf("%s **%s**\n%s", icon, title, body)})
	case "slack":
		payload, _ = json.Marshal(map[string]any{"text": fmt.Sprintf("%s *%s*\n%s", icon, title, body)})
	case "ntfy":
		payload = []byte(body)
		contentType = "text/plain"
	default:
		payload, _ = json.Marshal(map[string]any{"source": "hardy-rmm", "severity": severity, "title": title, "message": body, "text": title + "\n" + body, "content": title + "\n" + body})
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, st.WebhookURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if st.WebhookFormat == "ntfy" {
		req.Header.Set("Title", title)
		req.Header.Set("Priority", map[string]string{"critical": "urgent", "warning": "high", "info": "default"}[severity])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}
	return nil
}
