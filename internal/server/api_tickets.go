package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

var ticketStatuses = map[string]bool{"open": true, "in_progress": true, "waiting": true, "resolved": true, "closed": true}
var ticketPriorities = map[string]bool{"low": true, "medium": true, "high": true, "urgent": true}

func (s *Server) listTickets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	where := []string{"true"}
	args := []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	switch st := q.Get("status"); st {
	case "", "active":
		where = append(where, "t.status NOT IN ('resolved','closed')")
	case "all":
	default:
		add("t.status = ?", st)
	}
	if v := q.Get("client"); v != "" {
		add("t.client_id = ?", v)
	}
	if v := q.Get("device"); v != "" {
		add("t.device_id = ?", v)
	}
	if v := q.Get("priority"); v != "" {
		add("t.priority = ?", v)
	}
	switch v := q.Get("assignee"); v {
	case "":
	case "me":
		add("t.assignee_id = ?", userFrom(r).ID)
	case "none":
		where = append(where, "t.assignee_id IS NULL")
	default:
		add("t.assignee_id = ?", v)
	}
	if v := strings.TrimSpace(q.Get("q")); v != "" {
		if n, err := strconv.Atoi(strings.TrimPrefix(v, "#")); err == nil {
			add("t.number = ?", n)
		} else {
			add("(t.title ILIKE ? OR t.description ILIKE ? OR t.requester_name ILIKE ? OR t.requester_email ILIKE ?)", "%"+v+"%")
		}
	}
	rows, err := s.db.Maps(r.Context(), `SELECT t.*, c.name AS client_name, coalesce(nullif(d.display_name,''), d.hostname) AS device_name,
		coalesce(nullif(u.name,''), u.email) AS assignee_name,
		(SELECT count(*) FROM ticket_comments tc WHERE tc.ticket_id=t.id AND tc.kind='comment') AS comment_count,
		(SELECT coalesce(sum(time_minutes),0) FROM ticket_comments tc WHERE tc.ticket_id=t.id) AS time_minutes
		FROM tickets t LEFT JOIN clients c ON c.id=t.client_id LEFT JOIN devices d ON d.id=t.device_id LEFT JOIN users u ON u.id=t.assignee_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY CASE WHEN t.status IN ('resolved','closed') THEN 1 ELSE 0 END,
		         CASE t.priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END, t.updated_at DESC LIMIT 500`, args...)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) getTicket(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	t, err := s.db.Map(r.Context(), `SELECT t.*, c.name AS client_name, coalesce(nullif(d.display_name,''), d.hostname) AS device_name, d.online AS device_online,
		coalesce(nullif(u.name,''), u.email) AS assignee_name
		FROM tickets t LEFT JOIN clients c ON c.id=t.client_id LEFT JOIN devices d ON d.id=t.device_id LEFT JOIN users u ON u.id=t.assignee_id WHERE t.id=$1`, id)
	if err != nil {
		fail(w, err)
		return
	}
	t["comments"], _ = s.db.Maps(r.Context(), `SELECT * FROM ticket_comments WHERE ticket_id=$1 ORDER BY created_at`, id)
	t["alerts"], _ = s.db.Maps(r.Context(), `SELECT id, title, severity, status, created_at FROM alerts WHERE ticket_id=$1`, id)
	writeJSON(w, 200, t)
}

type ticketIn struct {
	Title          *string `json:"title"`
	Description    *string `json:"description"`
	Status         *string `json:"status"`
	Priority       *string `json:"priority"`
	Category       *string `json:"category"`
	ClientID       *string `json:"client_id"`
	DeviceID       *string `json:"device_id"`
	AssigneeID     *string `json:"assignee_id"`
	RequesterName  *string `json:"requester_name"`
	RequesterEmail *string `json:"requester_email"`
	DueAt          *string `json:"due_at"`
}

func strOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

func nullable(p *string) *string {
	if p == nil || *p == "" {
		return nil
	}
	return p
}

func (s *Server) createTicket(w http.ResponseWriter, r *http.Request) {
	var in ticketIn
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	title := strings.TrimSpace(strOr(in.Title, ""))
	if title == "" {
		fail(w, badRequest("title is required"))
		return
	}
	prio := strOr(in.Priority, "medium")
	if !ticketPriorities[prio] {
		prio = "medium"
	}
	u := userFrom(r)
	ctx := r.Context()
	clientID := nullable(in.ClientID)
	if clientID == nil && nullable(in.DeviceID) != nil {
		var c *string
		_ = s.db.QueryRow(ctx, `SELECT client_id FROM devices WHERE id=$1`, *in.DeviceID).Scan(&c)
		clientID = c
	}
	due, err := parseDue(in.DueAt)
	if err != nil {
		fail(w, err)
		return
	}
	var id string
	err = s.db.QueryRow(ctx, `INSERT INTO tickets (title, description, priority, category, client_id, device_id, assignee_id, requester_name, requester_email, due_at, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		title, strOr(in.Description, ""), prio, strOr(in.Category, ""), clientID, nullable(in.DeviceID), nullable(in.AssigneeID),
		strOr(in.RequesterName, ""), strOr(in.RequesterEmail, ""), due, u.Display()).Scan(&id)
	if err != nil {
		fail(w, badRequest("could not create ticket: "+err.Error()))
		return
	}
	s.ticketEvent(r, id, "Ticket created")
	s.audit(ctx, u, r, "ticket.created", "ticket", id, map[string]any{"title": title})
	r2 := r.WithContext(ctx)
	chi.RouteContext(r2.Context()).URLParams.Add("id", id)
	s.getTicket(w, r2)
}

func parseDue(p *string) (*time.Time, error) {
	if p == nil || *p == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, *p, time.Local); err == nil {
			return &t, nil
		}
	}
	return nil, badRequest("invalid due date")
}

func (s *Server) ticketEvent(r *http.Request, ticketID, body string) {
	u := userFrom(r)
	_, _ = s.db.Exec(r.Context(), `INSERT INTO ticket_comments (ticket_id, author_id, author_name, body, internal, kind) VALUES ($1,$2,$3,$4,true,'event')`,
		ticketID, u.ID, u.Display(), body)
}

func (s *Server) updateTicket(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var in ticketIn
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	ctx := r.Context()
	cur, err := s.db.Map(ctx, `SELECT t.status, t.priority, t.assignee_id, coalesce(nullif(u.name,''), u.email) AS assignee_name FROM tickets t LEFT JOIN users u ON u.id=t.assignee_id WHERE t.id=$1`, id)
	if err != nil {
		fail(w, err)
		return
	}
	set := []string{"updated_at=now()"}
	args := []any{id}
	add := func(col string, v any) {
		args = append(args, v)
		set = append(set, fmt.Sprintf("%s=$%d", col, len(args)))
	}
	var events []string
	if in.Title != nil && strings.TrimSpace(*in.Title) != "" {
		add("title", strings.TrimSpace(*in.Title))
	}
	if in.Description != nil {
		add("description", *in.Description)
	}
	if in.Category != nil {
		add("category", *in.Category)
	}
	if in.Status != nil && ticketStatuses[*in.Status] && *in.Status != cur["status"] {
		add("status", *in.Status)
		if *in.Status == "resolved" || *in.Status == "closed" {
			set = append(set, "resolved_at=coalesce(resolved_at, now())")
		} else {
			set = append(set, "resolved_at=NULL")
		}
		events = append(events, fmt.Sprintf("Status changed from %s to %s", cur["status"], *in.Status))
	}
	if in.Priority != nil && ticketPriorities[*in.Priority] && *in.Priority != cur["priority"] {
		add("priority", *in.Priority)
		events = append(events, fmt.Sprintf("Priority changed from %s to %s", cur["priority"], *in.Priority))
	}
	if in.ClientID != nil {
		add("client_id", nullable(in.ClientID))
	}
	if in.DeviceID != nil {
		add("device_id", nullable(in.DeviceID))
	}
	if in.AssigneeID != nil {
		add("assignee_id", nullable(in.AssigneeID))
		var name string
		if *in.AssigneeID != "" {
			_ = s.db.QueryRow(ctx, `SELECT coalesce(nullif(name,''), email) FROM users WHERE id=$1`, *in.AssigneeID).Scan(&name)
			events = append(events, "Assigned to "+name)
		} else {
			events = append(events, "Unassigned")
		}
	}
	if in.RequesterName != nil {
		add("requester_name", *in.RequesterName)
	}
	if in.RequesterEmail != nil {
		add("requester_email", *in.RequesterEmail)
	}
	if in.DueAt != nil {
		due, err := parseDue(in.DueAt)
		if err != nil {
			fail(w, err)
			return
		}
		add("due_at", due)
	}
	if _, err := s.db.Exec(ctx, `UPDATE tickets SET `+strings.Join(set, ", ")+` WHERE id=$1`, args...); err != nil {
		fail(w, badRequest("update failed: "+err.Error()))
		return
	}
	for _, e := range events {
		s.ticketEvent(r, id, e)
	}
	if in.Status != nil && (*in.Status == "resolved" || *in.Status == "closed") {
		_, _ = s.db.Exec(ctx, `UPDATE alerts SET status='resolved', resolved_at=now() WHERE ticket_id=$1 AND status<>'resolved'`, id)
	}
	s.audit(ctx, userFrom(r), r, "ticket.updated", "ticket", id, map[string]any{"changes": events})
	s.getTicket(w, r)
}

func (s *Server) addComment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var in struct {
		Body        string `json:"body"`
		Internal    bool   `json:"internal"`
		TimeMinutes int    `json:"time_minutes"`
		Status      string `json:"status"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	if strings.TrimSpace(in.Body) == "" && in.TimeMinutes == 0 {
		fail(w, badRequest("comment is empty"))
		return
	}
	u := userFrom(r)
	ctx := r.Context()
	if _, err := s.db.Exec(ctx, `INSERT INTO ticket_comments (ticket_id, author_id, author_name, body, internal, time_minutes) VALUES ($1,$2,$3,$4,$5,$6)`,
		id, u.ID, u.Display(), in.Body, in.Internal, in.TimeMinutes); err != nil {
		fail(w, badRequest("ticket not found"))
		return
	}
	_, _ = s.db.Exec(ctx, `UPDATE tickets SET updated_at=now() WHERE id=$1`, id)
	if in.Status != "" {
		st := in.Status
		s.updateTicketStatus(r, id, st)
	}
	s.getTicket(w, r)
}

func (s *Server) updateTicketStatus(r *http.Request, id, status string) {
	if !ticketStatuses[status] {
		return
	}
	var cur string
	_ = s.db.QueryRow(r.Context(), `SELECT status FROM tickets WHERE id=$1`, id).Scan(&cur)
	if cur == status {
		return
	}
	resolved := "NULL"
	if status == "resolved" || status == "closed" {
		resolved = "coalesce(resolved_at, now())"
	}
	_, _ = s.db.Exec(r.Context(), `UPDATE tickets SET status=$2, resolved_at=`+resolved+`, updated_at=now() WHERE id=$1`, id, status)
	s.ticketEvent(r, id, fmt.Sprintf("Status changed from %s to %s", cur, status))
}

func (s *Server) deleteTicket(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	_, _ = s.db.Exec(r.Context(), `UPDATE alerts SET ticket_id=NULL WHERE ticket_id=$1`, id)
	if _, err := s.db.Exec(r.Context(), `DELETE FROM tickets WHERE id=$1`, id); err != nil {
		fail(w, err)
		return
	}
	s.audit(r.Context(), userFrom(r), r, "ticket.deleted", "ticket", id, nil)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
