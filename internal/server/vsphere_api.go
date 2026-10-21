package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hardynetworks/apex-rmm/internal/vsphere"
	"github.com/jackc/pgx/v5"
)

// ---- connections ----

const vsListSQL = `SELECT c.id, c.name, c.url, c.username, (c.password<>'') AS password_set, c.insecure, c.thumbprint,
	c.client_id, cl.name AS client_name, c.enabled, c.poll_seconds, c.ds_warn_pct, c.ds_crit_pct, c.snapshot_days,
	c.alert_alarms, c.alert_hosts, c.create_tickets, c.notify, c.status, c.last_error, c.last_poll, c.about, c.created_at,
	(SELECT count(*) FROM alerts a WHERE starts_with(a.source_key, 'vsphere:' || c.id || ':') AND a.status<>'resolved') AS open_alerts
	FROM vsphere_connections c LEFT JOIN clients cl ON cl.id=c.client_id `

func (s *Server) listVSphere(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Maps(r.Context(), vsListSQL+`ORDER BY lower(c.name)`)
	if err != nil {
		fail(w, err)
		return
	}
	for _, row := range rows {
		vsRedact(row, userFrom(r))
	}
	writeJSON(w, 200, rows)
}

// vsRedact hides endpoint details from non-admins.
func vsRedact(row map[string]any, u *User) {
	if u != nil && roleRank(u.Role) >= roleRank("admin") {
		return
	}
	for _, k := range []string{"url", "username", "thumbprint", "last_error"} {
		delete(row, k)
	}
}

type vsIn struct {
	Name          string  `json:"name"`
	URL           string  `json:"url"`
	Username      string  `json:"username"`
	Password      string  `json:"password"` // empty = keep the stored one
	Insecure      bool    `json:"insecure"`
	Thumbprint    string  `json:"thumbprint"`
	ClientID      *string `json:"client_id"`
	Enabled       bool    `json:"enabled"`
	PollSeconds   int     `json:"poll_seconds"`
	DSWarnPct     int     `json:"ds_warn_pct"`
	DSCritPct     int     `json:"ds_crit_pct"`
	SnapshotDays  int     `json:"snapshot_days"`
	AlertAlarms   bool    `json:"alert_alarms"`
	AlertHosts    bool    `json:"alert_hosts"`
	CreateTickets bool    `json:"create_tickets"`
	Notify        bool    `json:"notify"`
}

func (in *vsIn) validate(creating bool) error {
	in.Name = strings.TrimSpace(in.Name)
	in.URL = strings.TrimSpace(in.URL)
	in.Username = strings.TrimSpace(in.Username)
	in.Thumbprint = strings.TrimSpace(in.Thumbprint)
	if in.Name == "" {
		return badRequest("name is required")
	}
	if _, err := vsphere.ParseURL(in.URL); err != nil {
		return badRequest(err.Error())
	}
	if in.Username == "" {
		return badRequest("username is required")
	}
	if creating && in.Password == "" {
		return badRequest("password is required")
	}
	if in.ClientID != nil && *in.ClientID == "" {
		in.ClientID = nil
	}
	if in.PollSeconds < 30 || in.PollSeconds > 3600 {
		return badRequest("poll interval must be between 30 and 3600 seconds")
	}
	if in.DSWarnPct < 1 || in.DSWarnPct > 100 || in.DSCritPct < 1 || in.DSCritPct > 100 || in.DSWarnPct >= in.DSCritPct {
		return badRequest("datastore thresholds must be 1-100% with warning below critical")
	}
	if in.SnapshotDays < 0 || in.SnapshotDays > 365 {
		return badRequest("snapshot age must be between 0 and 365 days (0 disables the alert)")
	}
	return nil
}

func (s *Server) saveVSphere(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in vsIn
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	id := chi.URLParam(r, "id")
	if err := in.validate(id == ""); err != nil {
		fail(w, err)
		return
	}
	var err error
	if id == "" {
		err = s.db.QueryRow(ctx, `INSERT INTO vsphere_connections (name, url, username, password, insecure, thumbprint, client_id, enabled,
			poll_seconds, ds_warn_pct, ds_crit_pct, snapshot_days, alert_alarms, alert_hosts, create_tickets, notify)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id`,
			in.Name, in.URL, in.Username, s.box.seal(in.Password), in.Insecure, in.Thumbprint, in.ClientID, in.Enabled,
			in.PollSeconds, in.DSWarnPct, in.DSCritPct, in.SnapshotDays, in.AlertAlarms, in.AlertHosts, in.CreateTickets, in.Notify).Scan(&id)
	} else {
		pw := ""
		if in.Password != "" {
			pw = s.box.seal(in.Password)
		}
		var tag int64
		tag, err = execCount(ctx, s.db, `UPDATE vsphere_connections SET name=$2, url=$3, username=$4, password=CASE WHEN $5 <> '' THEN $5 ELSE password END,
			insecure=$6, thumbprint=$7, client_id=$8, enabled=$9, poll_seconds=$10, ds_warn_pct=$11, ds_crit_pct=$12, snapshot_days=$13,
			alert_alarms=$14, alert_hosts=$15, create_tickets=$16, notify=$17, last_poll=NULL WHERE id=$1`,
			id, in.Name, in.URL, in.Username, pw, in.Insecure, in.Thumbprint, in.ClientID, in.Enabled,
			in.PollSeconds, in.DSWarnPct, in.DSCritPct, in.SnapshotDays, in.AlertAlarms, in.AlertHosts, in.CreateTickets, in.Notify)
		if err == nil && tag == 0 {
			err = ErrNotFound
		}
	}
	if err != nil {
		if strings.Contains(err.Error(), "unique") {
			fail(w, conflict("a connection with that name already exists"))
			return
		}
		if strings.Contains(err.Error(), "foreign key") {
			fail(w, badRequest("unknown client"))
			return
		}
		fail(w, err)
		return
	}
	s.vs.drop(id) // settings changed: sign in again with the new ones
	s.audit(ctx, userFrom(r), r, "vsphere.connection_saved", "vsphere_connection", id, map[string]any{"name": in.Name, "url": in.URL, "username": in.Username, "password_changed": in.Password != ""})
	if in.Enabled {
		go func() { _ = s.vs.poll(context.Background(), id) }()
	}
	row, err := s.db.Map(ctx, vsListSQL+`WHERE c.id=$1`, id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, row)
}

func execCount(ctx context.Context, db *DB, sql string, args ...any) (int64, error) {
	tag, err := db.Exec(ctx, sql, args...)
	return tag.RowsAffected(), err
}

func (s *Server) deleteVSphere(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var name string
	if err := s.db.QueryRow(r.Context(), `DELETE FROM vsphere_connections WHERE id=$1 RETURNING name`, id).Scan(&name); err != nil {
		fail(w, ErrNotFound)
		return
	}
	s.vs.drop(id)
	_, _ = s.db.Exec(r.Context(), `UPDATE alerts SET status='resolved', resolved_at=now() WHERE starts_with(source_key, $1) AND status<>'resolved'`, vsAlertPrefix(id))
	s.audit(r.Context(), userFrom(r), r, "vsphere.connection_deleted", "vsphere_connection", id, map[string]any{"name": name})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// testVSphere signs in with the values from the form (or the stored password)
// and reports what it found, without saving anything.
func (s *Server) testVSphere(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID         string `json:"id"`
		URL        string `json:"url"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		Insecure   bool   `json:"insecure"`
		Thumbprint string `json:"thumbprint"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	if in.Password == "" && in.ID != "" {
		c, err := s.vsLoad(r.Context(), in.ID)
		if err != nil {
			fail(w, err)
			return
		}
		in.Password = c.Password
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	res, err := vsphere.Probe(ctx, vsphere.Config{URL: in.URL, Username: strings.TrimSpace(in.Username), Password: in.Password, Insecure: in.Insecure, Thumbprint: strings.TrimSpace(in.Thumbprint)})
	if err != nil {
		out := map[string]any{"ok": false, "error": err.Error()}
		var ce *vsphere.CertError
		if errors.As(err, &ce) {
			out["cert_untrusted"] = true
			out["thumbprint"] = ce.Thumbprint
		}
		writeJSON(w, 200, out)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "about": res.About, "hosts": res.Hosts, "vms": res.VMs, "datastores": res.Datastores})
}

// refreshVSphere polls a connection right now.
func (s *Server) refreshVSphere(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Minute)
	defer cancel()
	if err := s.vs.poll(ctx, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			fail(w, err)
			return
		}
		if errors.Is(err, errVsBusy) {
			fail(w, conflict(err.Error()))
			return
		}
		fail(w, badRequest(err.Error()))
		return
	}
	s.vsphereInventory(w, r)
}

// ---- inventory ----

type agentLink struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	Online   bool   `json:"online"`
	OS       string `json:"os"`
	Match    string `json:"match"` // uuid | ip | hostname
}

type vmView struct {
	vsphere.VM
	Agent *agentLink `json:"agent,omitempty"`
	UIURL string     `json:"ui_url"`
}

type hostView struct {
	vsphere.Host
	CPUPct float64 `json:"cpu_pct"`
	MemPct float64 `json:"mem_pct"`
	UIURL  string  `json:"ui_url"`
}

type dsView struct {
	vsphere.Datastore
	UsedPct float64 `json:"used_pct"`
}

func (s *Server) loadInventory(ctx context.Context, id string) (*vsphere.Inventory, time.Time, error) {
	var raw []byte
	var at time.Time
	if err := s.db.QueryRow(ctx, `SELECT data, updated_at FROM vsphere_inventory WHERE connection_id=$1`, id).Scan(&raw, &at); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, time.Time{}, nil
		}
		return nil, time.Time{}, err
	}
	var inv vsphere.Inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		return nil, time.Time{}, err
	}
	return &inv, at, nil
}

func (s *Server) vsphereInventory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	conn, err := s.db.Map(ctx, vsListSQL+`WHERE c.id=$1`, id)
	if err != nil {
		fail(w, err)
		return
	}
	conn0 := map[string]any{"url": conn["url"]}
	inv, at, err := s.loadInventory(ctx, id)
	if err != nil {
		fail(w, err)
		return
	}
	vsRedact(conn, userFrom(r))
	out := map[string]any{"connection": conn, "inventory": nil}
	if inv != nil {
		endpoint, _ := vsphere.ParseURL(conn0["url"].(string))
		links := s.vsAgentLinks(ctx, inv.VMs)
		hosts := make([]hostView, 0, len(inv.Hosts))
		for _, h := range inv.Hosts {
			v := hostView{Host: h, CPUPct: h.CPUPct(), MemPct: h.MemPct()}
			if endpoint != nil {
				v.UIURL = vsphere.HostLink(endpoint, inv.About, h.Ref)
			}
			hosts = append(hosts, v)
		}
		vms := make([]vmView, 0, len(inv.VMs))
		for _, vm := range inv.VMs {
			v := vmView{VM: vm}
			if l, ok := links[vm.Ref]; ok {
				l := l
				v.Agent = &l
			}
			if endpoint != nil {
				v.UIURL = vsphere.VMLink(endpoint, inv.About, vm.Ref)
			}
			vms = append(vms, v)
		}
		dss := make([]dsView, 0, len(inv.Datastores))
		for _, d := range inv.Datastores {
			dss = append(dss, dsView{Datastore: d, UsedPct: d.UsedPct()})
		}
		alarms := inv.Alarms
		if alarms == nil {
			alarms = []vsphere.Alarm{}
		}
		out["inventory"] = map[string]any{"about": inv.About, "collected_at": inv.CollectedAt, "updated_at": at, "hosts": hosts, "vms": vms, "datastores": dss, "alarms": alarms}
	}
	writeJSON(w, 200, out)
}

// vsAgentLinks finds the Apex agents installed in the inventory's VMs.
func (s *Server) vsAgentLinks(ctx context.Context, vms []vsphere.VM) map[string]agentLink {
	rows, err := s.db.Maps(ctx, `SELECT id, hostname, display_name, os, serial, online, local_ips FROM devices`)
	if err != nil {
		return nil
	}
	devs := make([]agentDev, 0, len(rows))
	for _, r := range rows {
		devs = append(devs, agentDev{
			ID: r["id"].(string), Hostname: r["hostname"].(string), DisplayName: r["display_name"].(string), OS: r["os"].(string),
			Serial: r["serial"].(string), Online: r["online"] == true, IPs: toStrings(r["local_ips"]),
		})
	}
	return matchAgents(vms, devs)
}

// ---- agent matching (pure) ----

type agentDev struct {
	ID, Hostname, DisplayName, OS, Serial string
	Online                                bool
	IPs                                   []string
}

var nonHex = regexp.MustCompile(`[^0-9a-f]`)

// uuidKey reduces a BIOS UUID, or a "VMware-42 02 1e ..." serial number, to 32 hex digits.
func uuidKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "vmware-")
	s = strings.TrimPrefix(s, "vmware ")
	k := nonHex.ReplaceAllString(s, "")
	if len(k) != 32 {
		return ""
	}
	return k
}

func shortHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

// matchAgents pairs VMs with enrolled devices: first by BIOS UUID (the device
// serial number of a VMware guest), then by IP address, then by host name. A
// device is never paired with two VMs, and ambiguous matches are skipped.
func matchAgents(vms []vsphere.VM, devs []agentDev) map[string]agentLink {
	out := map[string]agentLink{}
	used := map[string]bool{}
	link := func(vm vsphere.VM, d agentDev, how string) {
		name := d.DisplayName
		if name == "" {
			name = d.Hostname
		}
		out[vm.Ref] = agentLink{DeviceID: d.ID, Name: name, Online: d.Online, OS: d.OS, Match: how}
		used[d.ID] = true
	}

	bySerial := map[string]agentDev{}
	for _, d := range devs {
		if k := uuidKey(d.Serial); k != "" {
			bySerial[k] = d
		}
	}
	for _, vm := range vms {
		if d, ok := bySerial[uuidKey(vm.UUID)]; ok && uuidKey(vm.UUID) != "" && !used[d.ID] {
			link(vm, d, "uuid")
		}
	}

	byIP := map[string][]agentDev{}
	for _, d := range devs {
		if used[d.ID] {
			continue
		}
		for _, ip := range d.IPs {
			byIP[ip] = append(byIP[ip], d)
		}
	}
	for _, vm := range vms {
		if _, done := out[vm.Ref]; done {
			continue
		}
		cands := map[string]agentDev{}
		for _, ip := range vm.IPs {
			for _, d := range byIP[ip] {
				if !used[d.ID] {
					cands[d.ID] = d
				}
			}
		}
		if len(cands) == 1 {
			for _, d := range cands {
				link(vm, d, "ip")
			}
		}
	}

	hostDevs := map[string][]agentDev{}
	for _, d := range devs {
		if !used[d.ID] && d.Hostname != "" {
			hostDevs[shortHost(d.Hostname)] = append(hostDevs[shortHost(d.Hostname)], d)
		}
	}
	vmHosts := map[string]int{}
	for _, vm := range vms {
		if _, done := out[vm.Ref]; !done && vm.Hostname != "" {
			vmHosts[shortHost(vm.Hostname)]++
		}
	}
	for _, vm := range vms {
		if _, done := out[vm.Ref]; done || vm.Hostname == "" {
			continue
		}
		k := shortHost(vm.Hostname)
		if ds := hostDevs[k]; len(ds) == 1 && vmHosts[k] == 1 && !used[ds[0].ID] {
			link(vm, ds[0], "hostname")
		}
	}
	return out
}

// ---- VM power ----

var refRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

func (s *Server) vsphereVMPower(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ref := chi.URLParam(r, "id"), chi.URLParam(r, "ref")
	var in struct {
		Action string `json:"action"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	if _, ok := vsphere.PowerActions[in.Action]; !ok {
		fail(w, badRequest("unknown power action"))
		return
	}
	if !refRe.MatchString(ref) {
		fail(w, badRequest("invalid VM reference"))
		return
	}
	c, err := s.vsLoad(ctx, id)
	if err != nil {
		fail(w, err)
		return
	}
	inv, _, err := s.loadInventory(ctx, id)
	if err != nil {
		fail(w, err)
		return
	}
	var vm *vsphere.VM
	if inv != nil {
		for i := range inv.VMs {
			if inv.VMs[i].Ref == ref {
				vm = &inv.VMs[i]
			}
		}
	}
	if vm == nil || vm.Template {
		fail(w, badRequest("that VM is not in the inventory (or is a template)"))
		return
	}
	conn, err := s.vs.session(ctx, c)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	err = conn.PowerVM(ctx, ref, in.Action)
	s.audit(ctx, userFrom(r), r, "vsphere.vm_power", "vsphere_vm", ref, map[string]any{"connection": c.Name, "vm": vm.Name, "action": in.Action, "ok": err == nil})
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	// show the new power state soon
	go func() {
		time.Sleep(3 * time.Second)
		_ = s.vs.poll(context.Background(), id)
	}()
	writeJSON(w, 200, map[string]any{"ok": true, "vm": vm.Name, "action": in.Action})
}
