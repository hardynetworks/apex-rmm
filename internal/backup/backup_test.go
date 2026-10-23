package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

func init() { WorkFactor = 10 }

func testPool(t *testing.T) *pgxpool.Pool {
	dsn := os.Getenv("APEX_TEST_BACKUP_DATABASE_URL")
	if dsn == "" {
		t.Skip("APEX_TEST_BACKUP_DATABASE_URL not set (an empty database the tests may wipe)")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestDumpAndLoad(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	exec(t, pool, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	exec(t, pool, `CREATE TABLE parent (id bigserial PRIMARY KEY, name text NOT NULL, data jsonb, raw bytea, ts timestamptz, maybe text, tags text[])`)
	exec(t, pool, `CREATE TABLE child (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), parent_id bigint NOT NULL REFERENCES parent(id) ON DELETE CASCADE, self_id uuid REFERENCES child(id), note text)`)
	exec(t, pool, `CREATE TABLE calc (id int PRIMARY KEY, a int, doubled int GENERATED ALWAYS AS (a * 2) STORED)`)
	exec(t, pool, `CREATE TABLE sessions (id text PRIMARY KEY)`)
	exec(t, pool, `INSERT INTO parent (name, data, raw, ts, maybe, tags) VALUES
		('plain', '{"a":1}', '\x00ff10', '2026-01-02T03:04:05Z', NULL, '{x,y}'),
		('quotes "and", commas
and newlines ü', '[1,"two"]', '', now(), '', '{}'),
		('null-ish', NULL, NULL, NULL, '\N', NULL)`)
	exec(t, pool, `INSERT INTO child (id, parent_id, self_id, note) VALUES ('00000000-0000-0000-0000-000000000002', 1, NULL, 'b')`)
	exec(t, pool, `INSERT INTO child (id, parent_id, self_id, note) VALUES ('00000000-0000-0000-0000-000000000001', 2, '00000000-0000-0000-0000-000000000002', 'a')`)
	exec(t, pool, `INSERT INTO calc (id, a) VALUES (1, 21)`)
	exec(t, pool, `INSERT INTO sessions VALUES ('s1')`)

	var buf bytes.Buffer
	m, err := Dump(ctx, pool, &buf, "correct horse battery", map[string][]byte{"app_key": []byte("key-1")}, []string{"sessions"}, Manifest{AppVersion: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Tables) != 3 {
		t.Fatalf("tables: %+v", m.Tables)
	}
	if _, err := Dump(ctx, pool, io.Discard, "short", nil, nil, Manifest{}, nil); err == nil {
		t.Fatal("short passphrase accepted")
	}
	if _, err := Extract(bytes.NewReader(buf.Bytes()), "wrong passphrase!!", nil); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if _, err := Extract(bytes.NewReader([]byte("not a backup at all")), "correct horse battery", nil); err == nil {
		t.Fatal("garbage accepted")
	}
	trunc := buf.Bytes()[:buf.Len()-40]
	if _, err := Extract(bytes.NewReader(trunc), "correct horse battery", nil); err == nil {
		t.Fatal("truncated backup accepted")
	}
	ex, err := Extract(bytes.NewReader(buf.Bytes()), "correct horse battery", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	if string(ex.Files["app_key"]) != "key-1" || ex.Manifest.AppVersion != "test" {
		t.Fatalf("extract: %+v %v", ex.Manifest, ex.Files)
	}

	// the database moves on: rows change, a newer version adds a column and drops one
	exec(t, pool, `DELETE FROM parent WHERE id = 3`)
	exec(t, pool, `INSERT INTO parent (name) VALUES ('later'), ('later2')`)
	exec(t, pool, `UPDATE calc SET a = 1`)
	exec(t, pool, `ALTER TABLE parent ADD COLUMN added text NOT NULL DEFAULT 'dflt'`)
	exec(t, pool, `ALTER TABLE parent DROP COLUMN tags`)
	exec(t, pool, `CREATE TABLE newer (id int)`)
	exec(t, pool, `INSERT INTO newer VALUES (1)`)
	exec(t, pool, `INSERT INTO sessions VALUES ('s2')`)

	// a failing hook rolls everything back
	if _, err := Load(ctx, pool, ex, nil, func(pgx.Tx) error { return errors.New("nope") }); err == nil {
		t.Fatal("hook error ignored")
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM parent`).Scan(&n)
	if n != 4 {
		t.Fatalf("rollback: %d parents", n)
	}

	hooked := false
	res, err := Load(ctx, pool, ex, nil, func(tx pgx.Tx) error {
		hooked = true
		_, err := tx.Exec(ctx, `INSERT INTO newer VALUES (99)`)
		return err
	})
	if err != nil || !hooked {
		t.Fatal(err)
	}
	if res.Tables != 3 || res.Rows != 6 || len(res.Warnings) != 1 {
		t.Fatalf("result: %+v", res)
	}
	type row struct {
		id                int64
		name, data, added string
		raw               []byte
		ts                *time.Time
		maybe             *string
	}
	rows, _ := pool.Query(ctx, `SELECT id, name, coalesce(data::text, '<nil>'), raw, ts, maybe, added FROM parent ORDER BY id`)
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name, &r.data, &r.raw, &r.ts, &r.maybe, &r.added); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 3 || got[0].name != "plain" || got[0].data != `{"a": 1}` || !bytes.Equal(got[0].raw, []byte{0, 0xff, 0x10}) ||
		got[0].ts == nil || !got[0].ts.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) || got[0].maybe != nil || got[0].added != "dflt" {
		t.Fatalf("row 1: %+v", got)
	}
	if got[1].name != "quotes \"and\", commas\nand newlines ü" || got[1].maybe == nil || *got[1].maybe != "" || got[1].raw == nil || len(got[1].raw) != 0 {
		t.Fatalf("row 2: %+v", got[1])
	}
	if got[2].maybe == nil || *got[2].maybe != `\N` || got[2].raw != nil || got[2].data != "<nil>" {
		t.Fatalf("row 3: %+v", got[2])
	}
	var next int64
	_ = pool.QueryRow(ctx, `INSERT INTO parent (name) VALUES ('after') RETURNING id`).Scan(&next)
	if next != 4 {
		t.Fatalf("sequence continues at %d", next)
	}
	var doubled, sess, newer, fkCount int
	_ = pool.QueryRow(ctx, `SELECT doubled FROM calc WHERE id = 1`).Scan(&doubled)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&sess)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM newer`).Scan(&newer)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE contype = 'f'`).Scan(&fkCount)
	if doubled != 42 || sess != 0 || newer != 1 || fkCount != 2 {
		t.Fatalf("doubled %d sessions %d newer %d fks %d", doubled, sess, newer, fkCount)
	}
	// foreign keys are enforced again
	if _, err := pool.Exec(ctx, `INSERT INTO child (parent_id) VALUES (12345)`); err == nil {
		t.Fatal("foreign key not restored")
	}
}

func TestNames(t *testing.T) {
	n := NewName(time.Date(2026, 10, 23, 2, 15, 0, 0, time.UTC))
	if n != "apex-backup-20261023T021500Z.tar.gz.age" || !IsBackupName(n) {
		t.Fatal(n)
	}
	for _, bad := range []string{"apex-backup-x.tar.gz.age", "../" + n, "foo.txt", "apex-backup-20261023T021500Z.tar.gz"} {
		if IsBackupName(bad) {
			t.Fatal(bad)
		}
	}
}

func TestStore(t *testing.T) {
	backend := s3mem.New()
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	defer srv.Close()
	if err := backend.CreateBucket("apex"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := (S3Config{Endpoint: srv.URL + "/apex", Bucket: "apex", AccessKey: "a", SecretKey: "b"}).Validate(); err == nil {
		t.Fatal("endpoint with a path accepted")
	}
	missing, _ := NewStore(S3Config{Endpoint: srv.URL, Bucket: "nope", AccessKey: "a", SecretKey: "b", PathStyle: true, Region: "us-east-1"})
	if err := missing.Test(ctx); err == nil {
		t.Fatal("missing bucket passed")
	}
	st, err := NewStore(S3Config{Endpoint: srv.URL, Bucket: "apex", Prefix: "/rmm/", AccessKey: "a", SecretKey: "b", PathStyle: true, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Test(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, "f")
		_ = os.WriteFile(p, bytes.Repeat([]byte{byte(i)}, 100+i), 0o600)
		if _, err := st.Upload(ctx, NewName(base.Add(time.Duration(i)*time.Hour)), p); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "x"), []byte("x"), 0o600)
	if _, err := st.Upload(ctx, "notes.txt", filepath.Join(dir, "x")); err != nil {
		t.Fatal(err)
	}
	list, err := st.List(ctx)
	if err != nil || len(list) != 5 || list[0].Name != NewName(base.Add(4*time.Hour)) || list[0].Size != 104 || list[0].Key != "rmm/"+list[0].Name {
		t.Fatalf("list: %v %+v", err, list)
	}
	gone, err := st.Prune(ctx, 3)
	if err != nil || len(gone) != 2 {
		t.Fatalf("prune: %v %v", err, gone)
	}
	list, _ = st.List(ctx)
	if len(list) != 3 || list[2].Name != NewName(base.Add(2*time.Hour)) {
		t.Fatalf("after prune: %+v", list)
	}
	rc, size, err := st.Open(ctx, list[0].Key)
	if err != nil || size != 104 {
		t.Fatal(err, size)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if len(b) != 104 || b[0] != 4 {
		t.Fatal("content")
	}
	if _, _, err := st.Open(ctx, "rmm/notes.txt"); err == nil {
		t.Fatal("opened a non-backup object")
	}
	if err := st.Delete(ctx, "other/"+list[0].Name); err == nil {
		t.Fatal("deleted outside the prefix")
	}
	if err := st.Delete(ctx, list[0].Key); err != nil {
		t.Fatal(err)
	}
}
