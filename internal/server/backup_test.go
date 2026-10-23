package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/hardynetworks/apex-rmm/internal/backup"
)

func TestNextBackup(t *testing.T) {
	loc := time.FixedZone("X", -5*3600)
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, loc)
	set := BackupSettings{Enabled: true, Time: "02:30"}
	if n := nextBackup(set, BackupState{}, now); !n.Equal(time.Date(2026, 10, 24, 2, 30, 0, 0, loc)) {
		t.Fatal(n)
	}
	set.Time = "23:15"
	if n := nextBackup(set, BackupState{}, now); !n.Equal(time.Date(2026, 10, 23, 23, 15, 0, 0, loc)) {
		t.Fatal(n)
	}
	// missed while down: the slot after the last handled one is in the past, so it runs now
	last := time.Date(2026, 10, 20, 23, 16, 0, 0, loc)
	if n := nextBackup(set, BackupState{LastScheduled: &last}, now); !n.Equal(time.Date(2026, 10, 21, 23, 15, 0, 0, loc)) || !n.Before(now) {
		t.Fatal(n)
	}
	if nextBackup(BackupSettings{}, BackupState{}, now) != nil {
		t.Fatal("disabled schedule has a next run")
	}
}

func (e *itEnv) waitIdle() {
	e.t.Helper()
	for i := 0; i < 300; i++ {
		if e.s.backups.status() == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatal("backup/restore still running")
}

func TestBackupAndRestore(t *testing.T) {
	backup.WorkFactor = 10
	e := newITEnv(t)
	ctx := context.Background()
	clean := func() {
		_, _ = e.s.db.Exec(ctx, `DELETE FROM settings WHERE key IN ('backup','backup_state','legacy_app_keys')`)
	}
	clean()
	t.Cleanup(clean)
	restarts := make(chan struct{}, 4)
	e.s.restart = func() { restarts <- struct{}{} }

	fake := s3mem.New()
	_ = fake.CreateBucket("apex-backups")
	srv := httptest.NewServer(gofakes3.New(fake).Server())
	defer srv.Close()
	s3 := map[string]any{"endpoint": srv.URL, "region": "us-east-1", "bucket": "apex-backups", "prefix": "rmm", "access_key": "AK", "secret_key": "SK", "path_style": true}

	// data worth keeping: a vSphere connection (with a sealed password) and a device
	_, cm, _ := e.do("admin", "POST", "/api/vsphere/test", map[string]any{"url": e.sim, "username": "user", "password": "pass"})
	_, vm, raw := e.do("admin", "POST", "/api/vsphere/connections", map[string]any{"name": "backup-lab", "url": e.sim, "username": "user", "password": "pass",
		"thumbprint": cm["thumbprint"], "enabled": true, "poll_seconds": 300, "ds_warn_pct": 85, "ds_crit_pct": 95, "snapshot_days": 3})
	conn, _ := vm["id"].(string)
	if conn == "" {
		t.Fatalf("connection: %s", raw)
	}
	var dev string
	if err := e.s.db.QueryRow(ctx, `INSERT INTO devices (hostname, secret_hash) VALUES ('kept-pc', 'h1') RETURNING id`).Scan(&dev); err != nil {
		t.Fatal(err)
	}

	// settings are admin-only and validated
	if c, _, _ := e.do("technician", "GET", "/api/backup", nil); c != 403 {
		t.Fatalf("technician read backups: %d", c)
	}
	if c, _, _ := e.do("admin", "PUT", "/api/backup/settings", map[string]any{"s3": s3, "passphrase": "short", "keep": 5}); c != 400 {
		t.Fatal("short passphrase accepted")
	}
	if c, _, _ := e.do("admin", "PUT", "/api/backup/settings", map[string]any{"s3": s3, "keep": 5}); c != 400 {
		t.Fatal("no passphrase accepted")
	}
	if c, _, _ := e.do("admin", "PUT", "/api/backup/settings", map[string]any{"s3": s3, "passphrase": "a long backup passphrase", "time": "25:00", "keep": 5}); c != 400 {
		t.Fatal("bad time accepted")
	}
	c, m, raw := e.do("admin", "PUT", "/api/backup/settings", map[string]any{"s3": s3, "passphrase": "a long backup passphrase", "enabled": true, "time": "02:30", "keep": 2})
	if c != 200 || m["configured"] != true || m["secret_key_set"] != true || m["s3"].(map[string]any)["secret_key"] != "" || m["next_run"] == nil {
		t.Fatalf("save: %d %s", c, raw)
	}
	// blank secret = keep; the test uses the saved one
	s3NoSecret := map[string]any{}
	for k, v := range s3 {
		s3NoSecret[k] = v
	}
	s3NoSecret["secret_key"] = ""
	if _, m, raw := e.do("admin", "POST", "/api/backup/test", map[string]any{"s3": s3NoSecret}); m["ok"] != true {
		t.Fatalf("test: %s", raw)
	}
	if _, m, _ := e.do("admin", "POST", "/api/backup/test", map[string]any{"s3": map[string]any{"endpoint": srv.URL, "bucket": "missing", "access_key": "a", "secret_key": "b", "path_style": true, "region": "us-east-1"}}); m["ok"] != false {
		t.Fatal("missing bucket passed the test")
	}

	// three backups, keep 2
	for i := 0; i < 3; i++ {
		if c, _, raw := e.do("admin", "POST", "/api/backup/run", nil); c != 202 {
			t.Fatalf("run: %s", raw)
		}
		e.waitIdle()
		time.Sleep(1100 * time.Millisecond) // names have one-second resolution
	}
	_, m, raw = e.do("admin", "GET", "/api/backup", nil)
	st := m["state"].(map[string]any)
	if st["last_ok"] != true || st["last_name"] == nil || st["last_size"].(float64) <= 0 {
		t.Fatalf("state: %s", raw)
	}
	_, m, raw = e.do("admin", "GET", "/api/backup/list", nil)
	list := m["backups"].([]any)
	if len(list) != 2 {
		t.Fatalf("list: %s", raw)
	}
	newest := list[0].(map[string]any)["key"].(string)
	if c, _, _ := e.do("admin", "GET", "/api/backup/download?key="+newest, nil); c != 200 {
		t.Fatal("download")
	}

	// things change after the backup
	_, _ = e.s.db.Exec(ctx, `DELETE FROM devices WHERE id=$1`, dev)
	_, _ = e.s.db.Exec(ctx, `INSERT INTO devices (hostname, secret_hash) VALUES ('after-backup', 'h2')`)

	// restore from Settings
	if c, _, _ := e.do("admin", "POST", "/api/backup/restore", map[string]any{"key": newest}); c != 400 {
		t.Fatal("restore without confirmation")
	}
	if c, _, _ := e.do("admin", "POST", "/api/backup/restore", map[string]any{"key": "rmm/notes.txt", "confirm": "RESTORE"}); c != 400 {
		t.Fatal("restored a non-backup key")
	}
	if c, _, raw := e.do("admin", "POST", "/api/backup/restore", map[string]any{"key": newest, "confirm": "RESTORE", "passphrase": "the wrong passphrase"}); c != 202 {
		t.Fatalf("restore: %s", raw)
	}
	e.waitIdle()
	if _, m, _ := e.do("admin", "GET", "/api/backup/restore", nil); m["state"] != "failed" {
		t.Fatalf("wrong passphrase: %v", m)
	}
	if c, _, raw := e.do("admin", "POST", "/api/backup/restore", map[string]any{"key": newest, "confirm": "RESTORE"}); c != 202 {
		t.Fatalf("restore: %s", raw)
	}
	e.waitIdle()
	select {
	case <-restarts:
	case <-time.After(5 * time.Second):
		t.Fatal("no restart after restore")
	}
	var kept, after, sessions int
	_ = e.s.db.QueryRow(ctx, `SELECT count(*) FROM devices WHERE id=$1`, dev).Scan(&kept)
	_ = e.s.db.QueryRow(ctx, `SELECT count(*) FROM devices WHERE hostname='after-backup'`).Scan(&after)
	_ = e.s.db.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&sessions)
	if kept != 1 || after != 0 || sessions != 0 {
		t.Fatalf("after restore: kept %d after %d sessions %d", kept, after, sessions)
	}

	// a brand-new server with a different app key restores the same backup from the setup wizard
	t.Setenv("APP_KEY", "a-different-key")
	e.s.box = newSecretBox("")
	_, _ = e.s.db.Exec(ctx, `DELETE FROM users`)
	_, _ = e.s.db.Exec(ctx, `DELETE FROM settings WHERE key IN ('system','backup','backup_state','legacy_app_keys')`)
	e.s.setupCode = "SETUP123"
	req := map[string]any{"code": "SETUP123", "s3": s3}
	if c, _, _ := e.do("", "POST", "/api/setup/restore/list", map[string]any{"code": "WRONG", "s3": s3}); c != 403 {
		t.Fatal("wrong setup code accepted")
	}
	c, m, raw = e.do("", "POST", "/api/setup/restore/list", req)
	if c != 200 || len(m["backups"].([]any)) != 2 {
		t.Fatalf("setup list: %d %s", c, raw)
	}
	req["key"], req["passphrase"] = newest, "a long backup passphrase"
	if c, _, raw := e.do("", "POST", "/api/setup/restore", req); c != 202 {
		t.Fatalf("setup restore: %s", raw)
	}
	e.waitIdle()
	if _, m, _ := e.do("", "GET", "/api/setup/restore", nil); m["state"] != "done" {
		t.Fatalf("setup restore state: %v", m)
	}
	<-restarts
	// "restart": the new server loads the old key and can read the old secrets
	e.s.loadLegacyKeys(ctx)
	var sealed string
	_ = e.s.db.QueryRow(ctx, `SELECT password FROM vsphere_connections WHERE id=$1`, conn).Scan(&sealed)
	if pw, err := e.s.box.open(sealed); err != nil || pw != "pass" {
		t.Fatalf("vSphere password after restore: %q %v", pw, err)
	}
	if _, pass, err := e.s.openBackup(e.s.loadBackup(ctx)); err != nil || pass != "a long backup passphrase" {
		t.Fatalf("backup settings after restore: %v", err)
	}
	if e.s.setupNeeded(ctx) {
		t.Fatal("setup still needed after restoring users")
	}
	bst := e.s.loadBackupState(ctx)
	if bst.LastRestore == nil || bst.LastRestore.By != "setup wizard" || bst.LastRestore.Tables == 0 || bst.LastScheduled == nil {
		t.Fatalf("restore record: %+v", bst)
	}
	var audits int
	_ = e.s.db.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action='backup.restored'`).Scan(&audits)
	if audits == 0 {
		t.Fatal("restore not audited")
	}
	// setup is over: the wizard endpoints refuse
	if c, _, _ := e.do("", "POST", "/api/setup/restore/list", req); c != 409 {
		t.Fatalf("setup restore after setup: %d", c)
	}
	if len(e.s.box.legacyRaw) != 1 || e.s.box.legacyRaw[0] != "integration-test-key" {
		t.Fatalf("legacy keys: %v", e.s.box.legacyRaw)
	}
}
