package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// DB wraps the pgx pool with small helpers returning generic maps, which keeps
// the JSON API handlers compact.
type DB struct {
	*pgxpool.Pool
}

// OpenDB connects (retrying while Postgres starts) and applies the schema.
func OpenDB(ctx context.Context, url string) (*DB, error) {
	var pool *pgxpool.Pool
	var err error
	for i := 0; i < 30; i++ {
		pool, err = pgxpool.New(ctx, url)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				break
			}
			pool.Close()
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &DB{pool}, nil
}

// Maps runs a query and returns rows as maps.
func (db *DB) Maps(ctx context.Context, sql string, args ...any) ([]map[string]any, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out, nil
}

// Map returns a single row as a map, or ErrNotFound.
func (db *DB) Map(ctx context.Context, sql string, args ...any) (map[string]any, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	m, err := pgx.CollectOneRow(rows, pgx.RowToMap)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// Setting reads a JSON setting into v; returns false if missing.
func (db *DB) Setting(ctx context.Context, key string, v any) bool {
	var raw []byte
	if err := db.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&raw); err != nil {
		return false
	}
	return json.Unmarshal(raw, v) == nil
}

// SetSetting upserts a JSON setting.
func (db *DB) SetSetting(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, key, b)
	return err
}

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")
