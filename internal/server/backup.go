package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hardynetworks/apex-rmm/internal/backup"
	"github.com/hardynetworks/apex-rmm/internal/proto"
)

// ---- Backups to an S3-compatible bucket ----
//
// A backup is every table except sessions plus the app key, as a gzip'd tar
// encrypted with a passphrase (age/scrypt). Restoring replaces the database in
// one transaction and restarts the server (Docker brings it back). Secrets
// sealed with the backup's app key keep working on a server with a different
// key: the old key is kept in the "legacy_app_keys" setting.

// BackupSettings is the "backup" setting. SecretKey and Passphrase are sealed.
type BackupSettings struct {
	S3         backup.S3Config `json:"s3"`
	Passphrase string          `json:"passphrase"`
	Enabled    bool            `json:"enabled"` // scheduled daily backup
	Time       string          `json:"time"`    // HH:MM, server local time
	Keep       int             `json:"keep"`    // backups to keep in the bucket
}

// BackupState is the "backup_state" setting.
type BackupState struct {
	LastRun       *time.Time     `json:"last_run,omitempty"`
	LastOK        bool           `json:"last_ok"`
	LastError     string         `json:"last_error,omitempty"`
	LastName      string         `json:"last_name,omitempty"`
	LastSize      int64          `json:"last_size,omitempty"`
	LastSeconds   float64        `json:"last_seconds,omitempty"`
	LastTrigger   string         `json:"last_trigger,omitempty"` // manual | scheduled
	LastSuccess   *time.Time     `json:"last_success,omitempty"`
	LastScheduled *time.Time     `json:"last_scheduled,omitempty"` // scheduled slot last handled
	LastRestore   *RestoreRecord `json:"last_restore,omitempty"`
}

// RestoreRecord describes the last restore into this server.
type RestoreRecord struct {
	At       time.Time `json:"at"`
	Name     string    `json:"name"`
	By       string    `json:"by"`
	Created  time.Time `json:"created"`
	Version  string    `json:"version"`
	Tables   int       `json:"tables"`
	Rows     int64     `json:"rows"`
	Warnings []string  `json:"warnings"`
}

const (
	defaultBackupTime = "02:30"
	defaultBackupKeep = 14
)

var hhmm = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// backupRunner allows one backup or restore at a time.
type backupRunner struct {
	mu      sync.Mutex
	op      string // "" | backup | restore
	phase   string
	started time.Time
	// result of the last restore started from the setup wizard (no session yet)
	setupState, setupError string
	fromSetup              bool
}

func (b *backupRunner) begin(op string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.op != "" {
		return false
	}
	b.op, b.phase, b.started = op, "Starting", time.Now()
	return true
}

func (b *backupRunner) say(msg string) {
	b.mu.Lock()
	b.phase = msg
	b.mu.Unlock()
}

func (b *backupRunner) end() {
	b.mu.Lock()
	b.op, b.phase = "", ""
	b.mu.Unlock()
}

func (b *backupRunner) status() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.op == "" {
		return nil
	}
	return map[string]any{"op": b.op, "phase": b.phase, "started": b.started}
}

func (s *Server) loadBackup(ctx context.Context) BackupSettings {
	var st BackupSettings
	s.db.Setting(ctx, "backup", &st)
	if st.Time == "" {
		st.Time = defaultBackupTime
	}
	if st.Keep <= 0 {
		st.Keep = defaultBackupKeep
	}
	return st
}

func (s *Server) loadBackupState(ctx context.Context) BackupState {
	var st BackupState
	s.db.Setting(ctx, "backup_state", &st)
	return st
}

func (s *Server) updateBackupState(ctx context.Context, f func(*BackupState)) {
	st := s.loadBackupState(ctx)
	f(&st)
	if err := s.db.SetSetting(ctx, "backup_state", st); err != nil {
		slog.Error("backup state", "err", err)
	}
}

// openBackup returns the bucket settings and passphrase with secrets opened.
func (s *Server) openBackup(st BackupSettings) (backup.S3Config, string, error) {
	c := st.S3
	sk, err := s.box.open(c.SecretKey)
	if err != nil {
		return c, "", fmt.Errorf("the saved S3 secret key cannot be decrypted: %w", err)
	}
	c.SecretKey = sk
	pass, err := s.box.open(st.Passphrase)
	if err != nil {
		return c, "", fmt.Errorf("the saved backup passphrase cannot be decrypted: %w", err)
	}
	return c, pass, nil
}

func (s *Server) backupStore(ctx context.Context) (*backup.Store, string, error) {
	cfg, pass, err := s.openBackup(s.loadBackup(ctx))
	if err != nil {
		return nil, "", err
	}
	if cfg.Endpoint == "" {
		return nil, "", badRequest("backups are not set up yet - enter the S3 bucket in Settings → Backups")
	}
	st, err := backup.NewStore(cfg)
	if err != nil {
		return nil, "", badRequest(err.Error())
	}
	return st, pass, nil
}

// ---- running a backup ----

// runBackup makes a backup and uploads it; it returns the object name.
func (s *Server) runBackup(ctx context.Context, trigger string, u *User) (string, error) {
	start := time.Now()
	name, size, err := s.makeBackup(ctx)
	now := time.Now()
	s.updateBackupState(ctx, func(st *BackupState) {
		st.LastRun, st.LastOK, st.LastTrigger, st.LastSeconds = &now, err == nil, trigger, now.Sub(start).Seconds()
		st.LastError = ""
		if err != nil {
			st.LastError = err.Error()
			return
		}
		st.LastName, st.LastSize, st.LastSuccess = name, size, &now
	})
	if err != nil {
		slog.Error("backup failed", "trigger", trigger, "err", err)
		s.audit(ctx, u, nil, "backup.failed", "backup", "", map[string]any{"trigger": trigger, "error": err.Error()})
		if gs := s.generalSettings(ctx); gs.WebhookURL != "" {
			_ = s.notify(ctx, gs, "critical", "Apex backup failed", err.Error())
		}
		return "", err
	}
	slog.Info("backup uploaded", "name", name, "bytes", size, "trigger", trigger)
	s.audit(ctx, u, nil, "backup.created", "backup", name, map[string]any{"trigger": trigger, "size": size})
	return name, nil
}

func (s *Server) makeBackup(ctx context.Context) (string, int64, error) {
	if !s.backups.begin("backup") {
		return "", 0, conflict("a backup or restore is already running")
	}
	defer s.backups.end()
	set := s.loadBackup(ctx)
	store, pass, err := s.backupStore(ctx)
	if err != nil {
		return "", 0, err
	}
	if len(pass) < backup.MinPassphrase {
		return "", 0, badRequest("set a backup passphrase first")
	}
	f, err := os.CreateTemp("", "apex-backup-*.age")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(f.Name())
	files := map[string][]byte{}
	if s.box.key != "" {
		files["app_key"] = []byte(s.box.key)
	}
	host, _ := os.Hostname()
	m := backup.Manifest{AppVersion: proto.Version, CreatedAt: time.Now().UTC(), Hostname: host}
	if _, err := backup.Dump(ctx, s.db.Pool, f, pass, files, []string{"sessions"}, m, s.backups.say); err != nil {
		f.Close()
		return "", 0, err
	}
	if err := f.Close(); err != nil {
		return "", 0, err
	}
	name := backup.NewName(m.CreatedAt)
	s.backups.say("Uploading to the bucket")
	size, err := store.Upload(ctx, name, f.Name())
	if err != nil {
		return "", 0, fmt.Errorf("upload: %w", err)
	}
	s.backups.say("Removing old backups")
	if gone, err := store.Prune(ctx, set.Keep); err != nil {
		slog.Warn("removing old backups", "err", err)
	} else if len(gone) > 0 {
		slog.Info("removed old backups", "count", len(gone))
	}
	return name, size, nil
}

func (s *Server) generalSettings(ctx context.Context) Settings {
	var st Settings
	s.db.Setting(ctx, "general", &st)
	if st.WebhookFormat == "" {
		st.WebhookFormat = "generic"
	}
	return st
}

// nextBackup is the next scheduled slot after the last one handled (it can be
// in the past when the server was down at that time; it then runs right away).
func nextBackup(set BackupSettings, st BackupState, now time.Time) *time.Time {
	if !set.Enabled {
		return nil
	}
	h, m := 2, 30
	_, _ = fmt.Sscanf(set.Time, "%d:%d", &h, &m)
	from := now
	if st.LastScheduled != nil {
		from = st.LastScheduled.In(now.Location())
	}
	slot := time.Date(from.Year(), from.Month(), from.Day(), h, m, 0, 0, now.Location())
	if !slot.After(from) {
		slot = slot.AddDate(0, 0, 1)
	}
	return &slot
}

// backupLoop runs the scheduled daily backup. A slot missed while the server
// was down runs as soon as it is back.
func (s *Server) backupLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		set := s.loadBackup(ctx)
		if !set.Enabled || set.S3.Endpoint == "" {
			continue
		}
		st := s.loadBackupState(ctx)
		now := time.Now()
		next := nextBackup(set, st, now)
		if next == nil || now.Before(*next) {
			continue
		}
		s.updateBackupState(ctx, func(st *BackupState) { st.LastScheduled = &now })
		_, _ = s.runBackup(ctx, "scheduled", nil)
	}
}

// ---- restore ----

// restoreBackup downloads, checks and restores a backup. On success the server restarts.
func (s *Server) restoreBackup(ctx context.Context, store *backup.Store, key, pass, by string) (*RestoreRecord, error) {
	if !s.backups.begin("restore") {
		return nil, conflict("a backup or restore is already running")
	}
	defer s.backups.end()
	s.backups.say("Downloading the backup")
	rc, size, err := store.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "apex-restore-*.age")
	if err != nil {
		rc.Close()
		return nil, err
	}
	defer os.Remove(f.Name())
	_, err = io.Copy(f, &progressReader{r: rc, total: size, say: s.backups.say})
	rc.Close()
	if err == nil {
		_, err = f.Seek(0, 0)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("download: %w", err)
	}
	s.backups.say("Decrypting and checking the backup")
	ex, err := backup.Extract(f, pass, s.backups.say)
	f.Close()
	if err != nil {
		return nil, err
	}
	defer ex.Close()
	name := key[strings.LastIndex(key, "/")+1:]
	rec := &RestoreRecord{At: time.Now(), Name: name, By: by, Created: ex.Manifest.CreatedAt, Version: ex.Manifest.AppVersion}
	cur := s.loadBackupState(ctx)
	res, err := backup.Load(ctx, s.db.Pool, ex, s.backups.say, func(tx pgx.Tx) error {
		return s.afterRestoreLoad(ctx, tx, ex, rec, cur)
	})
	if err != nil {
		return nil, err
	}
	rec.Tables, rec.Rows, rec.Warnings = res.Tables, res.Rows, res.Warnings
	s.updateBackupState(ctx, func(st *BackupState) { st.LastRestore = rec })
	// the restored audit log gets an entry; the server restarts to pick up everything
	s.audit(ctx, nil, nil, "backup.restored", "backup", name, map[string]any{"by": by, "tables": res.Tables, "rows": res.Rows, "warnings": res.Warnings})
	slog.Info("backup restored; restarting", "name", name, "tables", res.Tables, "rows", res.Rows)
	s.backups.say("Restarting")
	go s.restart()
	return rec, nil
}

// afterRestoreLoad runs in the restore transaction: it keeps secrets sealed
// with the backup's app key readable and records the restore.
func (s *Server) afterRestoreLoad(ctx context.Context, tx pgx.Tx, ex *backup.Extracted, rec *RestoreRecord, cur BackupState) error {
	oldKey := strings.TrimSpace(string(ex.Files["app_key"]))
	oldBox := s.box
	if oldKey != "" {
		oldBox = &secretBox{aead: aeadFor(oldKey), key: oldKey}
	}
	keys := []string{oldKey}
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT value FROM settings WHERE key='legacy_app_keys'`).Scan(&raw); err == nil {
		var sealed []string
		_ = json.Unmarshal(raw, &sealed)
		for _, sv := range sealed {
			if k, err := oldBox.open(sv); err == nil {
				keys = append(keys, k)
			}
		}
	}
	keys = append(keys, s.box.legacyRaw...)
	seen := map[string]bool{"": true, s.box.key: true}
	var sealed []string
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			sealed = append(sealed, s.box.seal(k))
		}
	}
	if sealed == nil {
		sealed = []string{}
	}
	b, _ := json.Marshal(sealed)
	if _, err := tx.Exec(ctx, `INSERT INTO settings(key,value) VALUES('legacy_app_keys',$1) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, b); err != nil {
		return err
	}
	// keep this server's backup history (the backup's copy is older), record the
	// restore, and don't run a "missed" scheduled backup straight away
	st := cur
	now := time.Now()
	st.LastRestore, st.LastScheduled = rec, &now
	b, _ = json.Marshal(st)
	_, err := tx.Exec(ctx, `INSERT INTO settings(key,value) VALUES('backup_state',$1) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, b)
	return err
}

// loadLegacyKeys reads the app keys of servers whose backups were restored here.
func (s *Server) loadLegacyKeys(ctx context.Context) {
	var sealed []string
	if !s.db.Setting(ctx, "legacy_app_keys", &sealed) {
		return
	}
	var keys []string
	for _, sv := range sealed {
		if k, err := s.box.open(sv); err == nil {
			keys = append(keys, k)
		} else {
			slog.Warn("a restored backup's app key cannot be read", "err", err)
		}
	}
	s.box.setLegacy(keys)
}

type progressReader struct {
	r          io.Reader
	n, total   int64
	say        func(string)
	lastReport time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if time.Since(p.lastReport) > 500*time.Millisecond && p.total > 0 {
		p.lastReport = time.Now()
		p.say(fmt.Sprintf("Downloading the backup (%d%%)", p.n*100/p.total))
	}
	return n, err
}

// ---- API (admin) ----

type backupView struct {
	S3            backup.S3Config `json:"s3"`
	SecretKeySet  bool            `json:"secret_key_set"`
	PassphraseSet bool            `json:"passphrase_set"`
	Enabled       bool            `json:"enabled"`
	Time          string          `json:"time"`
	Keep          int             `json:"keep"`
	Configured    bool            `json:"configured"`
	State         BackupState     `json:"state"`
	Running       map[string]any  `json:"running"`
	NextRun       *time.Time      `json:"next_run"`
	ServerTime    time.Time       `json:"server_time"`
	ServerTZ      string          `json:"server_tz"`
}

func (s *Server) backupView(ctx context.Context) backupView {
	set := s.loadBackup(ctx)
	st := s.loadBackupState(ctx)
	now := time.Now()
	c := set.S3
	v := backupView{S3: c, SecretKeySet: c.SecretKey != "", PassphraseSet: set.Passphrase != "", Enabled: set.Enabled, Time: set.Time, Keep: set.Keep,
		State: st, Running: s.backups.status(), ServerTime: now, ServerTZ: now.Format("MST (UTC-07:00)")}
	v.S3.SecretKey = ""
	v.Configured = c.Endpoint != "" && c.SecretKey != "" && set.Passphrase != ""
	if v.Configured {
		v.NextRun = nextBackup(set, st, now)
	}
	return v
}

func (s *Server) getBackup(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.backupView(r.Context()))
}

type backupIn struct {
	S3         backup.S3Config `json:"s3"` // empty secret_key = keep the saved one
	Passphrase string          `json:"passphrase"`
	Enabled    bool            `json:"enabled"`
	Time       string          `json:"time"`
	Keep       int             `json:"keep"`
}

// mergeS3 fills a blank secret key from the saved settings.
func (s *Server) mergeS3(ctx context.Context, in backup.S3Config) (backup.S3Config, error) {
	in.Endpoint, in.Bucket, in.Region, in.AccessKey = strings.TrimSpace(in.Endpoint), strings.TrimSpace(in.Bucket), strings.TrimSpace(in.Region), strings.TrimSpace(in.AccessKey)
	if in.SecretKey == "" {
		saved, _, err := s.openBackup(s.loadBackup(ctx))
		if err != nil {
			return in, err
		}
		in.SecretKey = saved.SecretKey
	}
	return in, nil
}

func (s *Server) putBackupSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in backupIn
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	c, err := s.mergeS3(ctx, in.S3)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	if err := c.Validate(); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	if in.Time == "" {
		in.Time = defaultBackupTime
	}
	if !hhmm.MatchString(in.Time) {
		fail(w, badRequest("the backup time must look like 02:30"))
		return
	}
	if in.Keep < 1 || in.Keep > 1000 {
		fail(w, badRequest("keep between 1 and 1000 backups"))
		return
	}
	old := s.loadBackup(ctx)
	pass := old.Passphrase
	if in.Passphrase != "" {
		if len(in.Passphrase) < backup.MinPassphrase {
			fail(w, badRequest(fmt.Sprintf("the passphrase must be at least %d characters", backup.MinPassphrase)))
			return
		}
		pass = s.box.seal(in.Passphrase)
	}
	if pass == "" {
		fail(w, badRequest("choose a backup passphrase"))
		return
	}
	c.SecretKey = s.box.seal(c.SecretKey)
	set := BackupSettings{S3: c, Passphrase: pass, Enabled: in.Enabled, Time: in.Time, Keep: in.Keep}
	if err := s.db.SetSetting(ctx, "backup", set); err != nil {
		fail(w, err)
		return
	}
	if set.Enabled && (!old.Enabled || old.Time != set.Time) {
		// the first scheduled backup is the next time the clock reaches set.Time
		now := time.Now()
		s.updateBackupState(ctx, func(st *BackupState) { st.LastScheduled = &now })
	}
	s.audit(ctx, userFrom(r), r, "backup.settings", "backup", "", map[string]any{"endpoint": c.Endpoint, "bucket": c.Bucket, "prefix": c.Prefix, "enabled": set.Enabled, "time": set.Time, "keep": set.Keep, "passphrase_changed": in.Passphrase != ""})
	writeJSON(w, 200, s.backupView(ctx))
}

func (s *Server) testBackup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in struct {
		S3 backup.S3Config `json:"s3"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	c, err := s.mergeS3(ctx, in.S3)
	if err == nil {
		err = testBucket(ctx, c)
	}
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func testBucket(ctx context.Context, c backup.S3Config) error {
	st, err := backup.NewStore(c)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return st.Test(ctx)
}

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request) {
	store, _, err := s.backupStore(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	list, err := store.List(r.Context())
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{"backups": list})
}

func (s *Server) runBackupNow(w http.ResponseWriter, r *http.Request) {
	if s.backups.status() != nil {
		fail(w, conflict("a backup or restore is already running"))
		return
	}
	set := s.loadBackup(r.Context())
	if set.S3.Endpoint == "" || set.Passphrase == "" {
		fail(w, badRequest("save the bucket and passphrase first"))
		return
	}
	u := userFrom(r)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		_, _ = s.runBackup(ctx, "manual", u)
	}()
	// wait briefly so the UI sees it running
	for i := 0; i < 20 && s.backups.status() == nil; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *Server) restoreBackupHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in struct {
		Key        string `json:"key"`
		Passphrase string `json:"passphrase"` // empty = the saved one
		Confirm    string `json:"confirm"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	if in.Confirm != "RESTORE" {
		fail(w, badRequest(`type RESTORE to confirm`))
		return
	}
	store, pass, err := s.backupStore(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	if in.Passphrase != "" {
		pass = in.Passphrase
	}
	if !store.Owns(in.Key) {
		fail(w, badRequest("pick a backup from the list"))
		return
	}
	if s.backups.status() != nil {
		fail(w, conflict("a backup or restore is already running"))
		return
	}
	u := userFrom(r)
	s.audit(ctx, u, r, "backup.restore_started", "backup", in.Key, nil)
	s.startRestore(store, in.Key, pass, u.Display(), false)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// startRestore runs a restore in the background.
func (s *Server) startRestore(store *backup.Store, key, pass, by string, fromSetup bool) {
	s.backups.mu.Lock()
	s.backups.setupState, s.backups.setupError, s.backups.fromSetup = "running", "", fromSetup
	s.backups.mu.Unlock()
	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		close(done)
		_, err := s.restoreBackup(ctx, store, key, pass, by)
		s.backups.mu.Lock()
		if err != nil {
			s.backups.setupState, s.backups.setupError = "failed", err.Error()
		} else {
			s.backups.setupState = "done"
		}
		s.backups.mu.Unlock()
		if err != nil {
			slog.Error("restore failed", "key", key, "err", err)
			if !fromSetup {
				s.updateBackupState(context.Background(), func(st *BackupState) {
					st.LastRestore = &RestoreRecord{At: time.Now(), Name: key[strings.LastIndex(key, "/")+1:], By: by, Warnings: []string{"failed: " + err.Error()}}
				})
			}
		}
	}()
	<-done
	for i := 0; i < 20 && s.backups.status() == nil; i++ {
		time.Sleep(10 * time.Millisecond)
	}
}

// restoreStatus reports the restore in progress (or its failure) to the page that started it.
func (s *Server) restoreStatus(w http.ResponseWriter, r *http.Request) {
	s.backups.mu.Lock()
	if r.URL.Path == "/api/setup/restore" && !s.backups.fromSetup {
		s.backups.mu.Unlock()
		writeJSON(w, 200, map[string]any{"state": "idle"}) // nothing for anonymous callers unless setup started it
		return
	}
	out := map[string]any{"state": s.backups.setupState, "error": s.backups.setupError, "phase": s.backups.phase}
	if s.backups.setupState == "" {
		out["state"] = "idle"
	}
	s.backups.mu.Unlock()
	writeJSON(w, 200, out)
}

func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request) {
	store, _, err := s.backupStore(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	key := r.URL.Query().Get("key")
	rc, size, err := store.Open(r.Context(), key)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	defer rc.Close()
	s.audit(r.Context(), userFrom(r), r, "backup.downloaded", "backup", key, nil)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, key[strings.LastIndex(key, "/")+1:]))
	w.Header().Set("Content-Length", fmt.Sprint(size))
	_, _ = io.Copy(w, rc)
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	store, _, err := s.backupStore(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	key := r.URL.Query().Get("key")
	if err := store.Delete(r.Context(), key); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	s.audit(r.Context(), userFrom(r), r, "backup.deleted", "backup", key, nil)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- restore from the setup wizard (fresh install, no users yet) ----

type setupRestoreIn struct {
	Code       string          `json:"code"`
	S3         backup.S3Config `json:"s3"`
	Key        string          `json:"key"`
	Passphrase string          `json:"passphrase"`
}

func (s *Server) setupRestoreAllowed(w http.ResponseWriter, r *http.Request, in *setupRestoreIn) (*backup.Store, bool) {
	if err := decode(r, in); err != nil {
		fail(w, err)
		return nil, false
	}
	if !s.setupNeeded(r.Context()) {
		writeErr(w, http.StatusConflict, "setup has already been completed - restore from Settings → Backups instead")
		return nil, false
	}
	if !s.checkSetupCode(r, in.Code) {
		writeErr(w, http.StatusForbidden, "wrong setup code - find it in the server log (docker compose logs apex)")
		return nil, false
	}
	store, err := backup.NewStore(in.S3)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return nil, false
	}
	return store, true
}

func (s *Server) setupListBackups(w http.ResponseWriter, r *http.Request) {
	var in setupRestoreIn
	store, ok := s.setupRestoreAllowed(w, r, &in)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	list, err := store.List(ctx)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{"backups": list})
}

func (s *Server) setupRestore(w http.ResponseWriter, r *http.Request) {
	var in setupRestoreIn
	store, ok := s.setupRestoreAllowed(w, r, &in)
	if !ok {
		return
	}
	if !store.Owns(in.Key) {
		fail(w, badRequest("pick a backup from the list"))
		return
	}
	if in.Passphrase == "" {
		fail(w, badRequest("enter the backup passphrase"))
		return
	}
	if s.backups.status() != nil {
		fail(w, conflict("a restore is already running"))
		return
	}
	s.audit(r.Context(), nil, r, "backup.restore_started", "backup", in.Key, map[string]any{"from": "setup"})
	s.startRestore(store, in.Key, in.Passphrase, "setup wizard", true)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// defaultRestart exits so Docker's restart policy starts a fresh server.
func defaultRestart() {
	time.Sleep(1500 * time.Millisecond)
	slog.Info("exiting so the restored data is loaded fresh (Docker restarts the server)")
	os.Exit(0)
}
