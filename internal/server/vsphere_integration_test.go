package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hardynetworks/apex-rmm/internal/vsphere"
	"github.com/vmware/govmomi/simulator"
)

// Runs only when APEX_TEST_DATABASE_URL points at a scratch Postgres database.
type itEnv struct {
	t       *testing.T
	s       *Server
	h       http.Handler
	cookies map[string]string
	sim     string
}

func (e *itEnv) do(role, method, path string, body any) (int, map[string]any, []byte) {
	e.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.cookies[role]})
	if method != http.MethodGet {
		req.Header.Set("X-Apex-CSRF", "1")
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m, rec.Body.Bytes()
}

func newITEnv(t *testing.T) *itEnv {
	dsn := os.Getenv("APEX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("APEX_TEST_DATABASE_URL not set")
	}
	t.Setenv("APP_KEY", "integration-test-key")
	t.Setenv("PUBLIC_URL", "https://rmm.test")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DatabaseURL = dsn
	cfg.AppKeyFile = ""
	ctx := context.Background()
	s, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"vsphere_deploys", "vsphere_inventory", "vsphere_connections", "alerts", "audit_log", "sessions", "users", "enrollment_tokens"} {
		_, _ = s.db.Exec(ctx, "DELETE FROM "+tbl)
	}
	e := &itEnv{t: t, s: s, h: s.routes(), cookies: map[string]string{}}
	for _, role := range []string{"admin", "technician", "viewer"} {
		var uid string
		if err := s.db.QueryRow(ctx, `INSERT INTO users (email, name, username, role) VALUES ($1,$1,$1,$2) RETURNING id`, role+"@x.test", role).Scan(&uid); err != nil {
			t.Fatal(err)
		}
		tok := randToken(24)
		e.cookies[role] = tok
		if _, err := s.db.Exec(ctx, `INSERT INTO sessions (id, user_id, expires_at) VALUES ($1,$2, now()+interval '1 hour')`, sha256Hex(tok), uid); err != nil {
			t.Fatal(err)
		}
	}
	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatal(err)
	}
	model.Service.Listen = &url.URL{User: url.UserPassword("user", "pass")}
	model.Service.TLS = new(tls.Config)
	srv := model.Service.NewServer()
	t.Cleanup(func() { srv.Close(); model.Remove(); s.vs.closeAll() })
	e.sim = "https://" + srv.URL.Host
	return e
}

func (e *itEnv) pollNow(id string) {
	e.t.Helper()
	for i := 0; i < 50; i++ {
		err := e.s.vs.poll(context.Background(), id)
		if err == nil {
			return
		}
		if err != errVsBusy {
			e.t.Fatalf("poll: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	e.t.Fatal("poll stayed busy")
}

func (e *itEnv) openAlerts(prefix string) int {
	var n int
	_ = e.s.db.QueryRow(context.Background(), `SELECT count(*) FROM alerts WHERE starts_with(source_key,$1) AND resolved_at IS NULL`, prefix).Scan(&n)
	return n
}

func TestVSphereEndToEnd(t *testing.T) {
	e := newITEnv(t)
	ctx := context.Background()

	// untrusted certificate is reported with a thumbprint to pin
	code, m, _ := e.do("admin", "POST", "/api/vsphere/test", map[string]any{"url": e.sim, "username": "user", "password": "pass"})
	if code != 200 || m["ok"] == true || m["cert_untrusted"] != true || m["thumbprint"] == "" {
		t.Fatalf("expected untrusted cert, got %d %v", code, m)
	}
	thumb, _ := m["thumbprint"].(string)
	code, m, _ = e.do("admin", "POST", "/api/vsphere/test", map[string]any{"url": e.sim, "username": "user", "password": "pass", "thumbprint": thumb})
	if code != 200 || m["ok"] != true {
		t.Fatalf("pinned test failed: %d %v", code, m)
	}
	if c, _, _ := e.do("technician", "POST", "/api/vsphere/test", map[string]any{}); c != 403 {
		t.Errorf("technician test should be 403, got %d", c)
	}

	// create
	var clientID string
	_ = e.s.db.QueryRow(ctx, `SELECT id FROM clients LIMIT 1`).Scan(&clientID)
	conn := map[string]any{"name": "lab", "url": e.sim, "username": "user", "password": "pass", "thumbprint": thumb,
		"enabled": true, "poll_seconds": 300, "ds_warn_pct": 85, "ds_crit_pct": 95, "snapshot_days": 3,
		"alert_alarms": true, "alert_hosts": true, "client_id": clientID}
	code, m, raw := e.do("admin", "POST", "/api/vsphere/connections", conn)
	if code != 200 && code != 201 {
		t.Fatalf("create: %d %s", code, raw)
	}
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("no id in %s", raw)
	}
	var stored string
	_ = e.s.db.QueryRow(ctx, `SELECT password FROM vsphere_connections WHERE id=$1`, id).Scan(&stored)
	if !strings.HasPrefix(stored, "enc:") || strings.Contains(stored, "pass") && stored == "pass" {
		t.Errorf("password not sealed: %q", stored)
	}
	_, _, list := e.do("admin", "GET", "/api/vsphere/connections", nil)
	if bytes.Contains(list, []byte(`"pass"`)) || bytes.Contains(list, []byte(stored)) {
		t.Errorf("list leaks the password: %s", list)
	}

	e.pollNow(id)

	// inventory: admin sees links, viewer is redacted
	code, m, raw = e.do("admin", "GET", "/api/vsphere/connections/"+id+"/inventory", nil)
	if code != 200 {
		t.Fatalf("inventory %d %s", code, raw)
	}
	invm, _ := m["inventory"].(map[string]any)
	vms, _ := invm["vms"].([]any)
	if len(vms) == 0 {
		t.Fatalf("no VMs: %.300s", raw)
	}
	if first, _ := vms[0].(map[string]any); first["ui_url"] == "" || first["ui_url"] == nil {
		t.Errorf("missing vSphere client link: %v", first)
	}
	if c, _, _ := e.do("viewer", "GET", "/api/vsphere/connections/"+id+"/inventory", nil); c != 200 {
		t.Errorf("viewer inventory %d", c)
	}
	_, _, vlist := e.do("viewer", "GET", "/api/vsphere/connections", nil)
	if bytes.Contains(vlist, []byte(e.sim)) || bytes.Contains(vlist, []byte(thumb)) {
		t.Errorf("viewer sees connection details: %s", vlist)
	}

	// alert thresholds open and auto-resolve
	pfx := "vsphere:" + id + ":"
	if n := e.openAlerts(pfx); n != 0 {
		t.Logf("baseline alerts: %d", n)
	}
	base := e.openAlerts(pfx + "ds:")
	upd := map[string]any{}
	for k, v := range conn {
		upd[k] = v
	}
	upd["password"] = "" // keep the stored one
	upd["ds_warn_pct"] = 0
	if c, _, _ := e.do("admin", "PUT", "/api/vsphere/connections/"+id, upd); c != 400 {
		t.Errorf("invalid thresholds should be 400, got %d", c)
	}
	upd["ds_warn_pct"] = 80
	if code, _, raw = e.do("admin", "PUT", "/api/vsphere/connections/"+id, upd); code != 200 {
		t.Fatalf("update: %d %s", code, raw)
	}
	var pw2 string
	_ = e.s.db.QueryRow(ctx, `SELECT password FROM vsphere_connections WHERE id=$1`, id).Scan(&pw2)
	if pw2 != stored {
		t.Errorf("empty password should keep the stored one")
	}
	e.pollNow(id)

	// a full datastore opens an alert (with a readable name); a healthy one resolves it
	vc, err := e.s.vsLoad(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	full := &vsphere.Inventory{CollectedAt: time.Now(), Datastores: []vsphere.Datastore{{Ref: "ds-x", Name: "prod-ssd", Capacity: 1000, Free: 20, Accessible: true}}}
	e.s.reconcileSourceAlerts(ctx, vsAlertPrefix(id), vsphereAlerts(vc, full), false, false, vc.ClientID, vc.Name)
	if n := e.openAlerts(pfx + "ds:"); n != base+1 {
		t.Errorf("full datastore should open exactly one alert (before %d, after %d)", base, n)
	}
	e.s.reconcileSourceAlerts(ctx, vsAlertPrefix(id), vsphereAlerts(vc, full), false, false, vc.ClientID, vc.Name)
	if n := e.openAlerts(pfx + "ds:"); n != base+1 {
		t.Errorf("repeated polls must not duplicate the alert, open=%d", n)
	}
	var nm string
	if err := e.s.db.QueryRow(ctx, `SELECT source_name FROM alerts WHERE source_key=$1 AND resolved_at IS NULL`, pfx+"ds:ds-x:space").Scan(&nm); err != nil || nm != "prod-ssd" {
		t.Errorf("alert source name = %q (%v)", nm, err)
	}
	code, _, raw = e.do("viewer", "GET", "/api/alerts", nil)
	if code != 200 || !bytes.Contains(raw, []byte("prod-ssd")) {
		t.Errorf("alerts list should show the datastore alert by name: %d %.200s", code, raw)
	}
	e.pollNow(id) // real inventory is healthy again
	if n := e.openAlerts(pfx + "ds:"); n != base {
		t.Errorf("alert should auto-resolve, open=%d", n)
	}

	// power: technician ok, viewer forbidden
	ref, name := "", ""
	for _, v := range vms {
		vm := v.(map[string]any)
		if vm["power_state"] == "poweredOn" {
			ref, name = vm["ref"].(string), vm["name"].(string)
			break
		}
	}
	if ref == "" {
		t.Fatal("no powered-on VM in simulator")
	}
	path := "/api/vsphere/connections/" + id + "/vms/" + ref + "/power"
	if c, _, _ := e.do("viewer", "POST", path, map[string]any{"action": "off"}); c != 403 {
		t.Errorf("viewer power should be 403, got %d", c)
	}
	if c, _, raw := e.do("technician", "POST", path, map[string]any{"action": "bogus"}); c != 400 {
		t.Errorf("bogus action should be 400, got %d %s", c, raw)
	}
	if c, _, raw := e.do("technician", "POST", "/api/vsphere/connections/"+id+"/vms/vm-9999/power", map[string]any{"action": "off"}); c != 400 && c != 404 {
		t.Errorf("unknown VM should be rejected, got %d %s", c, raw)
	}
	if c, _, raw := e.do("technician", "POST", path, map[string]any{"action": "off"}); c != 200 {
		t.Fatalf("power off %s: %d %s", name, c, raw)
	}
	inv, _, _ := e.s.loadInventory(ctx, id)
	_ = inv
	e.pollNow(id)
	inv, _, _ = e.s.loadInventory(ctx, id)
	for _, v := range inv.VMs {
		if v.Ref == ref && v.PowerState != "poweredOff" {
			t.Errorf("%s should be powered off, is %s", name, v.PowerState)
		}
	}
	var n int
	_ = e.s.db.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action='vsphere.vm_power'`).Scan(&n)
	if n == 0 {
		t.Errorf("power action not audited")
	}

	// deploy: make two VMs eligible by editing the stored inventory
	var deployRefs []string
	for i := range inv.VMs {
		v := &inv.VMs[i]
		if v.PowerState == "poweredOff" || v.Template {
			continue
		}
		if len(deployRefs) < 2 {
			v.ToolsRunning = "guestToolsRunning"
			v.OSFamily = []string{"linux", "windows"}[len(deployRefs)]
			deployRefs = append(deployRefs, v.Ref)
		}
	}
	raw2, _ := json.Marshal(inv)
	_, _ = e.s.db.Exec(ctx, `UPDATE vsphere_inventory SET data=$2 WHERE connection_id=$1`, id, raw2)

	dpath := "/api/vsphere/connections/" + id + "/deploy"
	if c, _, _ := e.do("technician", "POST", dpath, map[string]any{"vms": deployRefs}); c != 403 {
		t.Errorf("technician deploy should be 403, got %d", c)
	}
	if c, _, raw := e.do("admin", "POST", dpath, map[string]any{"vms": deployRefs, "client_id": clientID}); c != 400 {
		t.Errorf("missing creds should be 400, got %d %s", c, raw)
	}
	e.s.vs.install = func(ctx context.Context, c *vsphere.Conn, vmRef, osFamily string, auth vsphere.GuestAuth, installURL string, timeout time.Duration) (*vsphere.InstallResult, error) {
		if !strings.HasPrefix(installURL, "https://rmm.test/install/") {
			t.Errorf("bad install URL %s", installURL)
		}
		if osFamily == "windows" {
			return &vsphere.InstallResult{ExitCode: 1, Log: "boom"}, nil
		}
		return &vsphere.InstallResult{ExitCode: 0, Log: "installed"}, nil
	}
	code, m, raw = e.do("admin", "POST", dpath, map[string]any{"vms": deployRefs, "client_id": clientID,
		"windows": map[string]any{"username": "Administrator", "password": "WinSecret!"},
		"linux":   map[string]any{"username": "root", "password": "LinSecret!"}})
	if code != 200 {
		t.Fatalf("deploy: %d %s", code, raw)
	}
	batch, _ := m["batch_id"].(string)
	deadline := time.Now().Add(15 * time.Second)
	var status map[string]string
	for time.Now().Before(deadline) {
		status = map[string]string{}
		rows, _ := e.s.db.Maps(ctx, `SELECT os_family, status FROM vsphere_deploys WHERE batch_id=$1`, batch)
		done := len(rows) > 0
		for _, r := range rows {
			status[r["os_family"].(string)] = r["status"].(string)
			if s := r["status"].(string); s == "queued" || s == "running" {
				done = false
			}
		}
		if done {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if status["linux"] != "success" || status["windows"] != "failed" {
		t.Errorf("deploy results wrong: %v", status)
	}
	time.Sleep(300 * time.Millisecond)
	var revoked bool
	_ = e.s.db.QueryRow(ctx, `SELECT revoked FROM enrollment_tokens WHERE description LIKE 'vSphere deploy%' ORDER BY created_at DESC LIMIT 1`).Scan(&revoked)
	if !revoked {
		t.Errorf("enrollment token should be revoked after the batch")
	}
	var leak int
	_ = e.s.db.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE details::text LIKE '%Secret!%'`).Scan(&leak)
	if leak != 0 {
		t.Errorf("guest credentials leaked into the audit log")
	}
	_ = e.s.db.QueryRow(ctx, `SELECT count(*) FROM vsphere_deploys WHERE log LIKE '%Secret!%' OR message LIKE '%Secret!%'`).Scan(&leak)
	if leak != 0 {
		t.Errorf("guest credentials leaked into deploy rows")
	}

	// delete resolves alerts and removes data
	if c, _, raw := e.do("admin", "DELETE", "/api/vsphere/connections/"+id, nil); c != 200 && c != 204 {
		t.Fatalf("delete: %d %s", c, raw)
	}
	if n := e.openAlerts(pfx); n != 0 {
		t.Errorf("alerts should be resolved after delete, %d open", n)
	}
	_ = e.s.db.QueryRow(ctx, `SELECT count(*) FROM vsphere_inventory WHERE connection_id=$1`, id).Scan(&leak)
	if leak != 0 {
		t.Errorf("inventory not removed")
	}
}
