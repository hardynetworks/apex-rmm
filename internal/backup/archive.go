package backup

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FormatVersion is bumped if the archive layout changes incompatibly.
const FormatVersion = 1

// Manifest describes a backup archive (manifest.json inside it).
type Manifest struct {
	Format     int       `json:"format"`
	AppVersion string    `json:"app_version"`
	CreatedAt  time.Time `json:"created_at"`
	Hostname   string    `json:"hostname,omitempty"`
	Tables     []Table   `json:"tables"`
	Files      []string  `json:"files"`
}

// Table is one table's data in the archive (db/<name>.csv).
type Table struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Rows    int64    `json:"rows"`
}

// WorkFactor is the scrypt work factor for the passphrase (2^18 ≈ 1 s). Tests lower it.
var WorkFactor = 18

// MinPassphrase is the shortest passphrase accepted for new backups.
const MinPassphrase = 12

// ErrPassphrase means the backup could not be decrypted with the passphrase given.
var ErrPassphrase = errors.New("wrong passphrase - this backup was encrypted with a different one")

// Progress receives short status messages while a backup or restore runs.
type Progress func(msg string)

func (p Progress) say(format string, a ...any) {
	if p != nil {
		p(fmt.Sprintf(format, a...))
	}
}

// Dump writes an encrypted archive of every table in the public schema
// (except those in exclude) plus files to w. The archive is a gzip'd tar
// encrypted with age using the passphrase, so it can also be opened with the
// age command-line tool: age -d backup.tar.gz.age > backup.tar.gz
func Dump(ctx context.Context, pool *pgxpool.Pool, w io.Writer, passphrase string, files map[string][]byte, exclude []string, m Manifest, progress Progress) (Manifest, error) {
	if len(passphrase) < MinPassphrase {
		return m, fmt.Errorf("the backup passphrase must be at least %d characters", MinPassphrase)
	}
	// one consistent snapshot of the whole database
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return m, err
	}
	defer tx.Rollback(ctx)
	cur, err := describe(ctx, tx)
	if err != nil {
		return m, err
	}
	if _, err := tx.Exec(ctx, "SELECT 1"); err != nil { // the snapshot starts at the first query
		return m, err
	}
	rcpt, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return m, err
	}
	rcpt.SetWorkFactor(WorkFactor)
	enc, err := age.Encrypt(w, rcpt)
	if err != nil {
		return m, err
	}
	gz, _ := gzip.NewWriterLevel(enc, gzip.DefaultCompression)
	tw := tar.NewWriter(gz)

	tmp, err := os.CreateTemp("", "apex-dump-*.csv")
	if err != nil {
		return m, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	m.Format = FormatVersion
	m.Tables, m.Files = nil, nil
	for i, t := range cur.order {
		if slices.Contains(exclude, t) {
			continue
		}
		progress.say("Saving table %d of %d (%s)", i+1, len(cur.order), t)
		cols := cur.cols[t]
		if _, err := tmp.Seek(0, 0); err != nil {
			return m, err
		}
		if err := tmp.Truncate(0); err != nil {
			return m, err
		}
		names := make([]string, len(cols))
		for j, c := range cols {
			names[j] = c.name
		}
		tag, err := tx.Conn().PgConn().CopyTo(ctx, tmp, fmt.Sprintf("COPY %s (%s) TO STDOUT WITH (FORMAT csv)", ident(t), idents(names)))
		if err != nil {
			return m, fmt.Errorf("read table %s: %w", t, err)
		}
		size, _ := tmp.Seek(0, io.SeekCurrent)
		if _, err := tmp.Seek(0, 0); err != nil {
			return m, err
		}
		if err := writeEntry(tw, "db/"+t+".csv", size, tmp); err != nil {
			return m, err
		}
		m.Tables = append(m.Tables, Table{Name: t, Columns: names, Rows: tag.RowsAffected()})
	}
	for name, b := range files {
		if !safeName.MatchString(name) {
			return m, fmt.Errorf("bad file name %q", name)
		}
		if err := writeEntry(tw, "files/"+name, int64(len(b)), strings.NewReader(string(b))); err != nil {
			return m, err
		}
		m.Files = append(m.Files, name)
	}
	slices.Sort(m.Files)
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := writeEntry(tw, "manifest.json", int64(len(mb)), strings.NewReader(string(mb))); err != nil {
		return m, err
	}
	progress.say("Finishing")
	if err := tw.Close(); err != nil {
		return m, err
	}
	if err := gz.Close(); err != nil {
		return m, err
	}
	return m, enc.Close()
}

func writeEntry(tw *tar.Writer, name string, size int64, r io.Reader) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size, ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	n, err := io.Copy(tw, r)
	if err == nil && n != size {
		err = fmt.Errorf("%s: wrote %d of %d bytes", name, n, size)
	}
	return err
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,100}$`)

// Extracted is a decrypted, unpacked archive in a temporary directory.
type Extracted struct {
	Dir      string
	Manifest Manifest
	Files    map[string][]byte
}

// Close removes the temporary directory.
func (e *Extracted) Close() { _ = os.RemoveAll(e.Dir) }

// Extract decrypts and unpacks an archive into a temporary directory. The
// whole archive is read (and so authenticated) before anything is restored.
func Extract(r io.Reader, passphrase string, progress Progress) (*Extracted, error) {
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, err
	}
	id.SetMaxWorkFactor(22)
	dec, err := age.Decrypt(bufio.NewReaderSize(r, 1<<20), id)
	if err != nil {
		var nm *age.NoIdentityMatchError
		if errors.As(err, &nm) {
			return nil, ErrPassphrase
		}
		if strings.Contains(err.Error(), "header") || strings.Contains(err.Error(), "intro") {
			return nil, errors.New("this file is not an Apex backup (or it is damaged)")
		}
		return nil, err
	}
	gz, err := gzip.NewReader(dec)
	if err != nil {
		return nil, fmt.Errorf("damaged backup: %w", err)
	}
	dir, err := os.MkdirTemp("", "apex-restore-*")
	if err != nil {
		return nil, err
	}
	ex := &Extracted{Dir: dir, Files: map[string][]byte{}}
	ok := false
	defer func() {
		if !ok {
			ex.Close()
		}
	}()
	_ = os.Mkdir(filepath.Join(dir, "db"), 0o700)
	tr := tar.NewReader(gz)
	haveManifest := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("damaged backup: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		switch {
		case h.Name == "manifest.json":
			b, err := io.ReadAll(io.LimitReader(tr, 4<<20))
			if err != nil {
				return nil, fmt.Errorf("damaged backup: %w", err)
			}
			if err := json.Unmarshal(b, &ex.Manifest); err != nil {
				return nil, fmt.Errorf("damaged backup manifest: %w", err)
			}
			haveManifest = true
		case strings.HasPrefix(h.Name, "files/") && safeName.MatchString(strings.TrimPrefix(h.Name, "files/")):
			b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
			if err != nil {
				return nil, fmt.Errorf("damaged backup: %w", err)
			}
			ex.Files[strings.TrimPrefix(h.Name, "files/")] = b
		case strings.HasPrefix(h.Name, "db/") && strings.HasSuffix(h.Name, ".csv") && safeName.MatchString(strings.TrimSuffix(strings.TrimPrefix(h.Name, "db/"), ".csv")):
			progress.say("Unpacking %s", strings.TrimSuffix(strings.TrimPrefix(h.Name, "db/"), ".csv"))
			f, err := os.OpenFile(filepath.Join(dir, h.Name), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
			if err != nil {
				return nil, err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return nil, fmt.Errorf("damaged backup: %w", err)
			}
		}
	}
	if !haveManifest {
		return nil, errors.New("damaged backup: no manifest")
	}
	if ex.Manifest.Format != FormatVersion {
		return nil, fmt.Errorf("this backup uses format %d, which this version of Apex cannot read - update Apex first", ex.Manifest.Format)
	}
	for _, t := range ex.Manifest.Tables {
		if !safeName.MatchString(t.Name) {
			return nil, fmt.Errorf("damaged backup: bad table name %q", t.Name)
		}
		if _, err := os.Stat(filepath.Join(dir, "db", t.Name+".csv")); err != nil {
			return nil, fmt.Errorf("damaged backup: table %s is missing", t.Name)
		}
	}
	ok = true
	return ex, nil
}

// LoadResult summarises a restore.
type LoadResult struct {
	Tables   int      `json:"tables"`
	Rows     int64    `json:"rows"`
	Warnings []string `json:"warnings"`
}

// Load replaces the contents of every table with the archive's data in a
// single transaction: on any error nothing changes. Tables that exist only in
// the database are emptied, tables and columns that exist only in the backup
// are skipped (with a warning), and columns missing from the backup get their
// defaults, so a backup from an older version restores into a newer one.
// beforeCommit runs inside the transaction after the data is loaded.
func Load(ctx context.Context, pool *pgxpool.Pool, ex *Extracted, progress Progress, beforeCommit func(pgx.Tx) error) (LoadResult, error) {
	res := LoadResult{Warnings: []string{}}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)
	cur, err := describe(ctx, tx)
	if err != nil {
		return res, err
	}
	// foreign keys are dropped while loading and re-created (and so checked) afterwards
	type fk struct{ table, name, def string }
	var fks []fk
	rows, err := tx.Query(ctx, `SELECT c.relname, con.conname, pg_get_constraintdef(con.oid)
		FROM pg_constraint con JOIN pg_class c ON c.oid = con.conrelid
		WHERE con.contype = 'f' AND con.connamespace = 'public'::regnamespace ORDER BY 1, 2`)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var f fk
		if err := rows.Scan(&f.table, &f.name, &f.def); err != nil {
			rows.Close()
			return res, err
		}
		fks = append(fks, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for _, f := range fks {
		if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", ident(f.table), ident(f.name))); err != nil {
			return res, err
		}
	}
	if len(cur.order) > 0 {
		if _, err := tx.Exec(ctx, "TRUNCATE "+idents(cur.order)+" RESTART IDENTITY"); err != nil {
			return res, err
		}
	}
	for i, t := range ex.Manifest.Tables {
		have, ok := cur.cols[t.Name]
		if !ok {
			res.Warnings = append(res.Warnings, fmt.Sprintf("table %s is not used by this version and was skipped", t.Name))
			continue
		}
		progress.say("Restoring table %d of %d (%s)", i+1, len(ex.Manifest.Tables), t.Name)
		types := map[string]string{}
		for _, c := range have {
			types[c.name] = c.typ
		}
		var common, extra []string
		for _, c := range t.Columns {
			if _, ok := types[c]; ok {
				common = append(common, c)
			} else {
				extra = append(extra, c)
			}
		}
		f, err := os.Open(filepath.Join(ex.Dir, "db", t.Name+".csv"))
		if err != nil {
			return res, err
		}
		var n int64
		if len(extra) == 0 {
			tag, err := tx.Conn().PgConn().CopyFrom(ctx, f, fmt.Sprintf("COPY %s (%s) FROM STDIN WITH (FORMAT csv)", ident(t.Name), idents(t.Columns)))
			f.Close()
			if err != nil {
				return res, fmt.Errorf("restore table %s: %w", t.Name, err)
			}
			n = tag.RowsAffected()
		} else {
			// load into a scratch table with every backup column as text, then copy the known columns across
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: columns %s are not used by this version and were skipped", t.Name, strings.Join(extra, ", ")))
			defs := make([]string, len(t.Columns))
			for j, c := range t.Columns {
				defs[j] = ident(c) + " text"
			}
			if _, err := tx.Exec(ctx, "CREATE TEMP TABLE _apex_restore ("+strings.Join(defs, ", ")+")"); err != nil {
				f.Close()
				return res, err
			}
			_, err := tx.Conn().PgConn().CopyFrom(ctx, f, fmt.Sprintf("COPY _apex_restore (%s) FROM STDIN WITH (FORMAT csv)", idents(t.Columns)))
			f.Close()
			if err != nil {
				return res, fmt.Errorf("restore table %s: %w", t.Name, err)
			}
			sel := make([]string, len(common))
			for j, c := range common {
				sel[j] = fmt.Sprintf("CAST(%s AS %s)", ident(c), types[c])
			}
			tag, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM _apex_restore", ident(t.Name), idents(common), strings.Join(sel, ", ")))
			if err != nil {
				return res, fmt.Errorf("restore table %s: %w", t.Name, err)
			}
			n = tag.RowsAffected()
			if _, err := tx.Exec(ctx, "DROP TABLE _apex_restore"); err != nil {
				return res, err
			}
		}
		res.Tables++
		res.Rows += n
	}
	progress.say("Checking links between tables")
	for _, f := range fks {
		if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s", ident(f.table), ident(f.name), f.def)); err != nil {
			return res, fmt.Errorf("the backup's data is inconsistent (%s.%s): %w", f.table, f.name, err)
		}
	}
	// serial counters continue after the restored rows
	seqs, err := tx.Query(ctx, `SELECT c.relname, a.attname, pg_get_serial_sequence(quote_ident(c.relname), a.attname)
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
		WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'r' AND a.attnum > 0 AND NOT a.attisdropped
		  AND pg_get_serial_sequence(quote_ident(c.relname), a.attname) IS NOT NULL`)
	if err != nil {
		return res, err
	}
	type sq struct{ table, col, seq string }
	var list []sq
	for seqs.Next() {
		var s sq
		if err := seqs.Scan(&s.table, &s.col, &s.seq); err != nil {
			seqs.Close()
			return res, err
		}
		list = append(list, s)
	}
	seqs.Close()
	for _, s := range list {
		if _, err := tx.Exec(ctx, fmt.Sprintf("SELECT setval($1, COALESCE((SELECT max(%s) FROM %s), 0) + 1, false)", ident(s.col), ident(s.table)), s.seq); err != nil {
			return res, err
		}
	}
	if beforeCommit != nil {
		if err := beforeCommit(tx); err != nil {
			return res, err
		}
	}
	progress.say("Saving")
	return res, tx.Commit(ctx)
}

type column struct{ name, typ string }

type schema struct {
	order []string
	cols  map[string][]column
}

// describe lists the public tables and their (non-generated) columns.
func describe(ctx context.Context, tx pgx.Tx) (*schema, error) {
	rows, err := tx.Query(ctx, `SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod)
		FROM pg_class c JOIN pg_attribute a ON a.attrelid = c.oid
		WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r', 'p') AND NOT c.relispartition
		  AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = ''
		ORDER BY c.relname, a.attnum`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	s := &schema{cols: map[string][]column{}}
	for rows.Next() {
		var t string
		var c column
		if err := rows.Scan(&t, &c.name, &c.typ); err != nil {
			return nil, err
		}
		if _, ok := s.cols[t]; !ok {
			s.order = append(s.order, t)
		}
		s.cols[t] = append(s.cols[t], c)
	}
	return s, rows.Err()
}

func ident(s string) string { return pgx.Identifier{s}.Sanitize() }

func idents(ss []string) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = ident(s)
	}
	return strings.Join(out, ", ")
}
