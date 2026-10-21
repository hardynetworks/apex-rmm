package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hardynetworks/apex-rmm/internal/vsphere"
	"github.com/jackc/pgx/v5"
)

// vsConn is a stored vCenter / ESXi connection (password already decrypted).
type vsConn struct {
	ID            string  `db:"id"`
	Name          string  `db:"name"`
	URL           string  `db:"url"`
	Username      string  `db:"username"`
	Password      string  `db:"password"`
	Insecure      bool    `db:"insecure"`
	Thumbprint    string  `db:"thumbprint"`
	ClientID      *string `db:"client_id"`
	Enabled       bool    `db:"enabled"`
	PollSeconds   int32   `db:"poll_seconds"`
	DSWarnPct     int32   `db:"ds_warn_pct"`
	DSCritPct     int32   `db:"ds_crit_pct"`
	SnapshotDays  int32   `db:"snapshot_days"`
	AlertAlarms   bool    `db:"alert_alarms"`
	AlertHosts    bool    `db:"alert_hosts"`
	CreateTickets bool    `db:"create_tickets"`
	Notify        bool    `db:"notify"`
}

const vsCols = `id, name, url, username, password, insecure, thumbprint, client_id, enabled, poll_seconds,
	ds_warn_pct, ds_crit_pct, snapshot_days, alert_alarms, alert_hosts, create_tickets, notify`

func (c *vsConn) vcfg() vsphere.Config {
	return vsphere.Config{URL: c.URL, Username: c.Username, Password: c.Password, Insecure: c.Insecure, Thumbprint: c.Thumbprint}
}

// fingerprint changes whenever something that affects the session changes.
func (c *vsConn) fingerprint() string {
	return sha256Hex(strings.Join([]string{c.URL, c.Username, c.Password, fmt.Sprint(c.Insecure), c.Thumbprint}, "\x00"))
}

// vsLoad reads one connection with its password decrypted.
func (s *Server) vsLoad(ctx context.Context, id string) (*vsConn, error) {
	rows, err := s.db.Query(ctx, `SELECT `+vsCols+` FROM vsphere_connections WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	c, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[vsConn])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	pw, err := s.box.open(c.Password)
	if err != nil {
		return nil, fmt.Errorf("stored vCenter password: %w", err)
	}
	c.Password = pw
	return &c, nil
}

type vsSession struct {
	conn *vsphere.Conn
	fp   string
}

// vsphereMgr polls every enabled connection, keeps one signed-in session per
// connection and turns inventory problems into alerts.
type vsphereMgr struct {
	s        *Server
	mu       sync.Mutex
	sessions map[string]*vsSession
	running  map[string]bool
	fails    map[string]int
	// install runs the installer inside a guest; replaced in tests.
	install func(ctx context.Context, conn *vsphere.Conn, vmRef, osFamily string, auth vsphere.GuestAuth, installURL string, timeout time.Duration) (*vsphere.InstallResult, error)
}

func newVsphereMgr(s *Server) *vsphereMgr {
	return &vsphereMgr{s: s, sessions: map[string]*vsSession{}, running: map[string]bool{}, fails: map[string]int{},
		install: func(ctx context.Context, conn *vsphere.Conn, vmRef, osFamily string, auth vsphere.GuestAuth, installURL string, timeout time.Duration) (*vsphere.InstallResult, error) {
			return conn.InstallAgent(ctx, vmRef, osFamily, auth, installURL, timeout)
		}}
}

var errVsBusy = errors.New("a refresh is already in progress; try again in a moment")

// session returns a signed-in session, reconnecting when the settings changed.
func (m *vsphereMgr) session(ctx context.Context, c *vsConn) (*vsphere.Conn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fp := c.fingerprint()
	if ss := m.sessions[c.ID]; ss != nil {
		if ss.fp == fp {
			return ss.conn, nil
		}
		ss.conn.Close()
		delete(m.sessions, c.ID)
	}
	conn, err := vsphere.Connect(ctx, c.vcfg())
	if err != nil {
		return nil, err
	}
	m.sessions[c.ID] = &vsSession{conn: conn, fp: fp}
	return conn, nil
}

func (m *vsphereMgr) drop(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ss := m.sessions[id]; ss != nil {
		ss.conn.Close()
		delete(m.sessions, id)
	}
	delete(m.fails, id)
}

func (m *vsphereMgr) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, ss := range m.sessions {
		ss.conn.Close()
		delete(m.sessions, id)
	}
}

// loop runs until ctx is cancelled.
func (m *vsphereMgr) loop(ctx context.Context) {
	// Deployments cannot survive a restart: do not leave them "running" forever.
	_, _ = m.s.db.Exec(ctx, `UPDATE vsphere_deploys SET status='failed', message='interrupted by a server restart', finished_at=now() WHERE status IN ('queued','running')`)
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		m.tick(ctx)
		select {
		case <-ctx.Done():
			m.closeAll()
			return
		case <-t.C:
		}
	}
}

func (m *vsphereMgr) tick(ctx context.Context) {
	rows, err := m.s.db.Query(ctx, `SELECT id FROM vsphere_connections WHERE enabled AND (last_poll IS NULL OR last_poll < now() - make_interval(secs => poll_seconds))`)
	if err != nil {
		return
	}
	ids, _ := pgx.CollectRows(rows, pgx.RowTo[string])
	for _, id := range ids {
		id := id
		go func() {
			if err := m.poll(ctx, id); err != nil && !errors.Is(err, errVsBusy) {
				slog.Warn("vsphere poll", "connection", id, "err", err)
			}
		}()
	}
}

func (m *vsphereMgr) begin(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running[id] {
		return false
	}
	m.running[id] = true
	return true
}

func (m *vsphereMgr) end(id string) {
	m.mu.Lock()
	delete(m.running, id)
	m.mu.Unlock()
}

// poll refreshes one connection's inventory and re-evaluates its alerts.
func (m *vsphereMgr) poll(ctx context.Context, id string) error {
	if !m.begin(id) {
		return errVsBusy
	}
	defer m.end(id)
	c, err := m.s.vsLoad(ctx, id)
	if err != nil {
		return err
	}
	conn, err := m.session(ctx, c)
	var inv *vsphere.Inventory
	if err == nil {
		inv, err = conn.Collect(ctx)
		if err != nil {
			m.dropSessionOnly(id) // force a fresh sign-in next time
		}
	}
	if err != nil {
		m.fail(ctx, c, err)
		return err
	}
	raw, _ := json.Marshal(inv)
	about, _ := json.Marshal(inv.About)
	tx, err := m.s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO vsphere_inventory (connection_id, data, updated_at) VALUES ($1,$2,now())
		ON CONFLICT (connection_id) DO UPDATE SET data=EXCLUDED.data, updated_at=now()`, id, raw); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE vsphere_connections SET status='ok', last_error='', last_poll=now(), about=$2 WHERE id=$1`, id, about); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.fails[id] = 0
	m.mu.Unlock()
	m.s.recordHostPower(ctx, id, inv)
	m.s.reconcileSourceAlerts(ctx, vsAlertPrefix(id), vsphereAlerts(c, inv), c.Notify, c.CreateTickets, c.ClientID, c.Name)
	return nil
}

// recordHostPower stores one history point per host that reports power draw,
// temperature or fan speed. Failures are ignored: history is best effort.
func (s *Server) recordHostPower(ctx context.Context, id string, inv *vsphere.Inventory) {
	for _, h := range inv.Hosts {
		var watts, temp, fan *float64
		watts = h.PowerWatts
		if t, ok := h.MaxTempC(); ok {
			temp = &t
		}
		if n, rpm := h.FanSummary(); n > 0 {
			fan = &rpm
		}
		if watts == nil && temp == nil && fan == nil {
			continue
		}
		_, _ = s.db.Exec(ctx, `INSERT INTO vsphere_host_power (connection_id, host_ref, ts, watts, temp_c, fan_min_rpm)
			VALUES ($1,$2,date_trunc('second', now()),$3,$4,$5) ON CONFLICT DO NOTHING`, id, h.Ref, watts, temp, fan)
	}
}

func (m *vsphereMgr) dropSessionOnly(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ss := m.sessions[id]; ss != nil {
		ss.conn.Close()
		delete(m.sessions, id)
	}
}

// fail records a failed poll; the second failure in a row raises an alert.
func (m *vsphereMgr) fail(ctx context.Context, c *vsConn, err error) {
	msg := err.Error()
	var ce *vsphere.CertError
	if errors.As(err, &ce) {
		msg = fmt.Sprintf("%s. Trust certificate %s under Virtualization → Connections.", err.Error(), ce.Thumbprint)
	}
	_, _ = m.s.db.Exec(ctx, `UPDATE vsphere_connections SET status='error', last_error=$2, last_poll=now() WHERE id=$1`, c.ID, msg)
	m.mu.Lock()
	m.fails[c.ID]++
	n := m.fails[c.ID]
	m.mu.Unlock()
	if n >= 2 {
		a := srcAlert{
			Key: vsAlertPrefix(c.ID) + "conn", Sev: "critical", Name: c.Name,
			Title: fmt.Sprintf("Cannot reach vSphere connection %q", c.Name),
			Msg:   fmt.Sprintf("Apex RMM could not read the inventory of %s (%s) for %d attempts in a row: %s", c.Name, c.URL, n, msg),
		}
		m.s.raiseSourceAlertIfMissing(ctx, a, c.Notify, c.CreateTickets, c.ClientID)
	}
}

func vsAlertPrefix(connID string) string { return "vsphere:" + connID + ":" }

// ---- alert rules (pure, unit tested) ----

// srcAlert is one condition that should be an open alert. Name is the object
// it concerns (shown where a device name would be).
type srcAlert struct {
	Key, Sev, Name, Title, Msg string
}

func vsSev(status string) string {
	if status == "red" {
		return "critical"
	}
	return "warning"
}

func fmtGB(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/(1<<30)) }

// vsphereAlerts derives the alerts that should currently be open for an inventory.
func vsphereAlerts(c *vsConn, inv *vsphere.Inventory) []srcAlert {
	p := vsAlertPrefix(c.ID)
	var out []srcAlert
	if c.AlertHosts {
		for _, h := range inv.Hosts {
			switch h.ConnectionState {
			case "notResponding":
				out = append(out, srcAlert{p + "host:" + h.Ref + ":conn", "critical", h.Name,
					h.Name + ": host is not responding",
					fmt.Sprintf("vCenter reports that ESXi host %s is not responding (connection %s). VMs on it may be unreachable.", h.Name, c.Name)})
			case "disconnected":
				out = append(out, srcAlert{p + "host:" + h.Ref + ":conn", "warning", h.Name,
					h.Name + ": host is disconnected",
					fmt.Sprintf("ESXi host %s is disconnected from vCenter (connection %s).", h.Name, c.Name)})
			}
			for _, s := range h.Sensors {
				reading := ""
				if s.Reading != "" {
					reading = " (reading " + s.Reading + ")"
				}
				out = append(out, srcAlert{p + "host:" + h.Ref + ":hw:" + s.Type + ":" + s.Name, vsSev(s.State), h.Name,
					fmt.Sprintf("%s: hardware %s is %s", h.Name, s.Name, s.State),
					fmt.Sprintf("Hardware sensor %q (%s) on ESXi host %s reports %s%s.", s.Name, s.Type, h.Name, s.State, reading)})
			}
		}
	}
	for _, d := range inv.Datastores {
		if !d.Accessible {
			out = append(out, srcAlert{p + "ds:" + d.Ref + ":access", "critical", d.Name,
				d.Name + ": datastore is inaccessible",
				fmt.Sprintf("Datastore %s is not accessible (connection %s).", d.Name, c.Name)})
			continue
		}
		if d.Capacity <= 0 {
			continue
		}
		pct := d.UsedPct()
		sev := ""
		switch {
		case c.DSCritPct > 0 && pct >= float64(c.DSCritPct):
			sev = "critical"
		case c.DSWarnPct > 0 && pct >= float64(c.DSWarnPct):
			sev = "warning"
		}
		if sev != "" {
			out = append(out, srcAlert{p + "ds:" + d.Ref + ":space", sev, d.Name,
				fmt.Sprintf("%s: datastore %.0f%% full", d.Name, pct),
				fmt.Sprintf("Datastore %s is %.1f%% full (%s free of %s). Thresholds: warning %d%%, critical %d%%.",
					d.Name, pct, fmtGB(d.Free), fmtGB(d.Capacity), c.DSWarnPct, c.DSCritPct)})
		}
	}
	if c.SnapshotDays > 0 {
		cutoff := inv.CollectedAt.Add(-time.Duration(c.SnapshotDays) * 24 * time.Hour)
		for _, v := range inv.VMs {
			if v.Template || v.Snapshots == 0 || v.OldestSnapshot == nil || !v.OldestSnapshot.Before(cutoff) {
				continue
			}
			days := int(inv.CollectedAt.Sub(*v.OldestSnapshot).Hours() / 24)
			out = append(out, srcAlert{p + "vm:" + v.Ref + ":snapshot", "warning", v.Name,
				fmt.Sprintf("%s: old snapshot (%d days)", v.Name, days),
				fmt.Sprintf("VM %s has %d snapshot(s); the oldest is %d days old. Old snapshots slow the VM down and grow until the datastore fills up.", v.Name, v.Snapshots, days)})
		}
	}
	if c.AlertAlarms {
		for _, a := range inv.Alarms {
			if a.Acknowledged {
				continue
			}
			out = append(out, srcAlert{p + "alarm:" + a.EntityType + ":" + a.EntityRef + ":" + a.AlarmRef, vsSev(a.Status), a.EntityName,
				fmt.Sprintf("%s: %s", a.EntityName, a.AlarmName),
				fmt.Sprintf("vCenter alarm %q is %s on %s %s (connection %s, since %s).", a.AlarmName, a.Status, a.EntityType, a.EntityName, c.Name, a.Time.Local().Format("2006-01-02 15:04"))})
		}
	}
	return out
}

// ---- alert storage for sources that are not agent devices ----

var sevRank = map[string]int{"info": 0, "warning": 1, "critical": 2}

// reconcileSourceAlerts opens alerts for new conditions, updates changed ones
// and resolves those that no longer apply, for all alerts under prefix.
func (s *Server) reconcileSourceAlerts(ctx context.Context, prefix string, want []srcAlert, notify, tickets bool, clientID *string, source string) {
	rows, err := s.db.Query(ctx, `SELECT id, source_key, severity FROM alerts WHERE starts_with(source_key, $1) AND status<>'resolved'`, prefix)
	if err != nil {
		slog.Error("reconcile alerts", "err", err)
		return
	}
	type open struct{ ID, Key, Sev string }
	existing := map[string]open{}
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.ID, &o.Key, &o.Sev); err == nil {
			existing[o.Key] = o
		}
	}
	rows.Close()

	wanted := map[string]bool{}
	for _, a := range want {
		if wanted[a.Key] {
			continue
		}
		wanted[a.Key] = true
		if o, ok := existing[a.Key]; ok {
			if o.Sev != a.Sev {
				_, _ = s.db.Exec(ctx, `UPDATE alerts SET severity=$2, title=$3, message=$4 WHERE id=$1`, o.ID, a.Sev, a.Title, a.Msg)
				if sevRank[a.Sev] > sevRank[o.Sev] && notify {
					s.sourceNotify(a)
				}
			} else {
				_, _ = s.db.Exec(ctx, `UPDATE alerts SET title=$2, message=$3 WHERE id=$1 AND (title<>$2 OR message<>$3)`, o.ID, a.Title, a.Msg)
			}
			continue
		}
		s.insertSourceAlert(ctx, a, notify, tickets, clientID)
	}
	for key, o := range existing {
		if !wanted[key] {
			_, _ = s.db.Exec(ctx, `UPDATE alerts SET status='resolved', resolved_at=now() WHERE id=$1`, o.ID)
		}
	}
}

// raiseSourceAlertIfMissing opens an alert unless one is already open for its key.
func (s *Server) raiseSourceAlertIfMissing(ctx context.Context, a srcAlert, notify, tickets bool, clientID *string) {
	var id string
	err := s.db.QueryRow(ctx, `SELECT id FROM alerts WHERE source_key=$1 AND status<>'resolved' LIMIT 1`, a.Key).Scan(&id)
	if err == nil {
		return
	}
	s.insertSourceAlert(ctx, a, notify, tickets, clientID)
}

func (s *Server) insertSourceAlert(ctx context.Context, a srcAlert, notify, tickets bool, clientID *string) {
	var id string
	if err := s.db.QueryRow(ctx, `INSERT INTO alerts (severity, title, message, source_key, source_name) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		a.Sev, a.Title, a.Msg, a.Key, a.Name).Scan(&id); err != nil {
		slog.Error("raise alert", "err", err)
		return
	}
	slog.Info("alert raised", "title", a.Title)
	if tickets {
		if tid, err := s.ticketForAlert(ctx, id, nil); err == nil && clientID != nil {
			_, _ = s.db.Exec(ctx, `UPDATE tickets SET client_id=$2 WHERE id=$1 AND client_id IS NULL`, tid, *clientID)
		}
	}
	if notify {
		s.sourceNotify(a)
	}
}

func (s *Server) sourceNotify(a srcAlert) {
	var st Settings
	s.db.Setting(context.Background(), "general", &st)
	if st.WebhookURL == "" {
		return
	}
	go func() {
		if err := s.notify(context.Background(), st, a.Sev, a.Title, a.Msg+"\n"+s.conf().PublicURL+"/virtualization"); err != nil {
			slog.Warn("webhook notify failed", "err", err)
		}
	}()
}
