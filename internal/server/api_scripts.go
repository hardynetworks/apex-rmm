package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/robfig/cron/v3"
)

var validShells = map[string]bool{"powershell": true, "pwsh": true, "cmd": true, "bash": true, "sh": true, "zsh": true, "python": true}

// ---- scripts ----

func (s *Server) listScripts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Maps(r.Context(), `SELECT id, name, description, category, shell, platforms, timeout_seconds, created_by, created_at, updated_at FROM scripts ORDER BY category, lower(name)`)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) getScript(w http.ResponseWriter, r *http.Request) {
	row, err := s.db.Map(r.Context(), `SELECT * FROM scripts WHERE id=$1`, chi.URLParam(r, "id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, row)
}

type scriptIn struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Shell       string   `json:"shell"`
	Platforms   []string `json:"platforms"`
	Body        string   `json:"body"`
	Timeout     int      `json:"timeout_seconds"`
}

func (s *Server) saveScript(w http.ResponseWriter, r *http.Request) {
	var in scriptIn
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Body) == "" {
		fail(w, badRequest("name and body are required"))
		return
	}
	if !validShells[in.Shell] {
		fail(w, badRequest("unsupported shell"))
		return
	}
	if in.Timeout <= 0 || in.Timeout > 86400 {
		in.Timeout = 300
	}
	if in.Category == "" {
		in.Category = "General"
	}
	u := userFrom(r)
	id := chi.URLParam(r, "id")
	ctx := r.Context()
	var err error
	if id == "" {
		err = s.db.QueryRow(ctx, `INSERT INTO scripts (name, description, category, shell, platforms, body, timeout_seconds, created_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
			in.Name, in.Description, in.Category, in.Shell, nz(in.Platforms), in.Body, in.Timeout, u.Display()).Scan(&id)
	} else {
		_, err = s.db.Exec(ctx, `UPDATE scripts SET name=$2, description=$3, category=$4, shell=$5, platforms=$6, body=$7, timeout_seconds=$8, updated_at=now() WHERE id=$1`,
			id, in.Name, in.Description, in.Category, in.Shell, nz(in.Platforms), in.Body, in.Timeout)
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.audit(ctx, u, r, "script.saved", "script", id, map[string]any{"name": in.Name})
	row, _ := s.db.Map(ctx, `SELECT * FROM scripts WHERE id=$1`, id)
	writeJSON(w, 200, row)
}

func (s *Server) deleteScript(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := s.db.Exec(r.Context(), `DELETE FROM scripts WHERE id=$1`, id); err != nil {
		fail(w, err)
		return
	}
	s.audit(r.Context(), userFrom(r), r, "script.deleted", "script", id, nil)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- jobs ----

type jobIn struct {
	ScriptID  string   `json:"script_id"`
	Name      string   `json:"name"`
	Shell     string   `json:"shell"`
	Body      string   `json:"body"`
	Args      []string `json:"args"`
	Timeout   int      `json:"timeout_seconds"`
	DeviceIDs []string `json:"device_ids"`
	ClientID  string   `json:"client_id"`
	All       bool     `json:"all"`
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var in jobIn
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	u := userFrom(r)
	jobID, n, err := s.launchJob(r.Context(), in, u.Display(), "manual", "")
	if err != nil {
		fail(w, err)
		return
	}
	s.audit(r.Context(), u, r, "job.created", "job", jobID, map[string]any{"name": in.Name, "script_id": in.ScriptID, "devices": n})
	s.getJobByID(w, r, jobID)
}

// launchJob resolves targets, creates results and dispatches to online agents.
func (s *Server) launchJob(ctx context.Context, in jobIn, by, source, scheduleID string) (string, int, error) {
	if in.ScriptID != "" {
		var shell, body, name string
		var timeout int
		err := s.db.QueryRow(ctx, `SELECT name, shell, body, timeout_seconds FROM scripts WHERE id=$1`, in.ScriptID).Scan(&name, &shell, &body, &timeout)
		if err != nil {
			return "", 0, badRequest("script not found")
		}
		in.Shell, in.Body = shell, body
		if in.Name == "" {
			in.Name = name
		}
		if in.Timeout == 0 {
			in.Timeout = timeout
		}
	}
	if !validShells[in.Shell] || strings.TrimSpace(in.Body) == "" {
		return "", 0, badRequest("shell and body are required")
	}
	if in.Timeout <= 0 || in.Timeout > 86400 {
		in.Timeout = 300
	}
	if in.Name == "" {
		in.Name = "Ad-hoc " + in.Shell + " command"
	}
	var devices []string
	var err error
	switch {
	case in.All:
		err = s.db.QueryRow(ctx, `SELECT coalesce(array_agg(id), '{}') FROM devices`).Scan(&devices)
	case in.ClientID != "":
		err = s.db.QueryRow(ctx, `SELECT coalesce(array_agg(id), '{}') FROM devices WHERE client_id=$1`, in.ClientID).Scan(&devices)
	default:
		err = s.db.QueryRow(ctx, `SELECT coalesce(array_agg(id), '{}') FROM devices WHERE id = ANY($1)`, nz(in.DeviceIDs)).Scan(&devices)
	}
	if err != nil {
		return "", 0, err
	}
	if len(devices) == 0 {
		return "", 0, badRequest("no target devices")
	}
	var scriptID, schedID *string
	if in.ScriptID != "" {
		scriptID = &in.ScriptID
	}
	if scheduleID != "" {
		schedID = &scheduleID
	}
	var jobID string
	err = s.db.QueryRow(ctx, `INSERT INTO jobs (name, script_id, schedule_id, shell, body, args, timeout_seconds, source, created_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		in.Name, scriptID, schedID, in.Shell, in.Body, nz(in.Args), in.Timeout, source, by).Scan(&jobID)
	if err != nil {
		return "", 0, err
	}
	for _, d := range devices {
		var rid string
		if err := s.db.QueryRow(ctx, `INSERT INTO job_results (job_id, device_id) VALUES ($1,$2) RETURNING id`, jobID, d).Scan(&rid); err != nil {
			return "", 0, err
		}
		if s.hub.Online(d) {
			s.sendScript(ctx, d, rid, in.Shell, in.Body, in.Args, in.Timeout)
		}
	}
	return jobID, len(devices), nil
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Maps(r.Context(), `SELECT j.id, j.name, j.shell, j.source, j.created_by, j.created_at,
		count(r.id) AS total,
		count(r.id) FILTER (WHERE r.status='success') AS success,
		count(r.id) FILTER (WHERE r.status IN ('failed','timeout','error','expired')) AS failed,
		count(r.id) FILTER (WHERE r.status IN ('pending','running')) AS active
		FROM jobs j LEFT JOIN job_results r ON r.job_id=j.id GROUP BY j.id ORDER BY j.created_at DESC LIMIT 200`)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	s.getJobByID(w, r, chi.URLParam(r, "id"))
}

func (s *Server) getJobByID(w http.ResponseWriter, r *http.Request, id string) {
	job, err := s.db.Map(r.Context(), `SELECT * FROM jobs WHERE id=$1`, id)
	if err != nil {
		fail(w, err)
		return
	}
	job["results"], _ = s.db.Maps(r.Context(), `SELECT r.*, coalesce(nullif(d.display_name,''), d.hostname) AS device_name, d.os, d.online
		FROM job_results r JOIN devices d ON d.id=r.device_id WHERE r.job_id=$1 ORDER BY device_name`, id)
	writeJSON(w, 200, job)
}

// ---- schedules ----

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Maps(r.Context(), `SELECT sc.*, s.name AS script_name, s.shell FROM schedules sc JOIN scripts s ON s.id=sc.script_id ORDER BY sc.name`)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) saveSchedule(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string   `json:"name"`
		ScriptID   string   `json:"script_id"`
		Cron       string   `json:"cron"`
		TargetType string   `json:"target_type"`
		TargetIDs  []string `json:"target_ids"`
		Enabled    bool     `json:"enabled"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	sched, err := cronParser.Parse(in.Cron)
	if err != nil {
		fail(w, badRequest("invalid cron expression: "+err.Error()))
		return
	}
	if in.TargetType != "devices" && in.TargetType != "client" && in.TargetType != "all" {
		fail(w, badRequest("target_type must be devices, client or all"))
		return
	}
	if in.Name == "" || in.ScriptID == "" {
		fail(w, badRequest("name and script are required"))
		return
	}
	next := sched.Next(time.Now())
	id := chi.URLParam(r, "id")
	u := userFrom(r)
	ctx := r.Context()
	if id == "" {
		err = s.db.QueryRow(ctx, `INSERT INTO schedules (name, script_id, cron, target_type, target_ids, enabled, next_run, created_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
			in.Name, in.ScriptID, in.Cron, in.TargetType, nz(in.TargetIDs), in.Enabled, next, u.Display()).Scan(&id)
	} else {
		_, err = s.db.Exec(ctx, `UPDATE schedules SET name=$2, script_id=$3, cron=$4, target_type=$5, target_ids=$6, enabled=$7, next_run=$8 WHERE id=$1`,
			id, in.Name, in.ScriptID, in.Cron, in.TargetType, nz(in.TargetIDs), in.Enabled, next)
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.audit(ctx, u, r, "schedule.saved", "schedule", id, map[string]any{"name": in.Name, "cron": in.Cron})
	row, _ := s.db.Map(ctx, `SELECT * FROM schedules WHERE id=$1`, id)
	writeJSON(w, 200, row)
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.Exec(r.Context(), `DELETE FROM schedules WHERE id=$1`, chi.URLParam(r, "id")); err != nil {
		fail(w, err)
		return
	}
	s.audit(r.Context(), userFrom(r), r, "schedule.deleted", "schedule", chi.URLParam(r, "id"), nil)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) runScheduleNow(w http.ResponseWriter, r *http.Request) {
	jobID, err := s.runSchedule(r.Context(), chi.URLParam(r, "id"), userFrom(r).Display())
	if err != nil {
		fail(w, err)
		return
	}
	s.getJobByID(w, r, jobID)
}

func (s *Server) runSchedule(ctx context.Context, id, by string) (string, error) {
	var name, scriptID, targetType string
	var targets []string
	if err := s.db.QueryRow(ctx, `SELECT name, script_id, target_type, target_ids FROM schedules WHERE id=$1`, id).Scan(&name, &scriptID, &targetType, &targets); err != nil {
		return "", ErrNotFound
	}
	in := jobIn{ScriptID: scriptID, Name: name}
	switch targetType {
	case "all":
		in.All = true
	case "client":
		if len(targets) == 0 {
			return "", badRequest("schedule has no client")
		}
		// multiple clients: expand to devices
		_ = s.db.QueryRow(ctx, `SELECT coalesce(array_agg(id), '{}') FROM devices WHERE client_id = ANY($1)`, targets).Scan(&in.DeviceIDs)
	default:
		in.DeviceIDs = targets
	}
	jobID, _, err := s.launchJob(ctx, in, by, "schedule", id)
	_, _ = s.db.Exec(ctx, `UPDATE schedules SET last_run=now() WHERE id=$1`, id)
	return jobID, err
}

func (s *Server) schedulerLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rows, err := s.db.Maps(ctx, `SELECT id, cron FROM schedules WHERE enabled AND next_run IS NOT NULL AND next_run <= now()`)
		if err != nil {
			continue
		}
		for _, row := range rows {
			id := row["id"].(string)
			if sched, err := cronParser.Parse(row["cron"].(string)); err == nil {
				_, _ = s.db.Exec(ctx, `UPDATE schedules SET next_run=$2 WHERE id=$1`, id, sched.Next(time.Now()))
			} else {
				_, _ = s.db.Exec(ctx, `UPDATE schedules SET enabled=false WHERE id=$1`, id)
				continue
			}
			if _, err := s.runSchedule(ctx, id, "scheduler"); err != nil {
				slog.Warn("scheduled run failed", "schedule", id, "err", err)
			}
		}
	}
}
