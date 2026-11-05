// Package storage is the SQLite database: connection setup, migrations and
// the small repositories the app needs.
//
// WAL mode lets readers run alongside the single writer; busy_timeout makes
// a second writer wait instead of failing. At a dozen users that's plenty.
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: static binary, no cgo

	"github.com/voidgrid/voidgrid-cleaner/migrations"
)

// DB is the open database.
type DB struct {
	*sql.DB
}

// Open opens (creating if needed, mode 0600) the database at path.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("storage: create database directory: %w", err)
	}
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_txlock", "immediate")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()

	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open database: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := d.PingContext(ctx); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("storage: open database %s (the file and its directory must be writable): %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("storage: restrict database file mode: %w", err)
	}
	return &DB{d}, nil
}

// Migrate applies, in filename order, every embedded migration not yet
// recorded in schema_migrations. Each file runs in its own transaction.
func (db *DB) Migrate(ctx context.Context) error {
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("storage: create schema_migrations: %w", err)
	}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("storage: read migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version = ?`, name).Scan(&n); err != nil {
			return fmt.Errorf("storage: check migration %s: %w", name, err)
		}
		if n > 0 {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("storage: read migration %s: %w", name, err)
		}
		if err := db.tx(ctx, func(tx *sql.Tx) error {
			// The driver runs every statement in the string.
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, name, time.Now().Unix())
			return err
		}); err != nil {
			return fmt.Errorf("storage: apply migration %s: %w", name, err)
		}
	}
	return nil
}

func (db *DB) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
