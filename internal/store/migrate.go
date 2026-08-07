package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrSchemaAhead is returned when the database records a migration this binary
// does not carry. It is the downgrade guard: a database written by a newer
// flowd must not be reinterpreted by an older one, because the older binary
// cannot know what the newer schema means.
var ErrSchemaAhead = errors.New("store: database schema is ahead of this binary")

// ErrSchemaUnknown is returned when the database records a migration version
// this binary does not carry and which is not simply newer — the migration set
// has diverged, so neither side can be trusted.
var ErrSchemaUnknown = errors.New("store: database records an unknown migration")

// Migration is one numbered schema file embedded in the binary.
type Migration struct {
	Version int
	Name    string
	File    string
	SQL     string
}

const migrationsDir = "migrations"

// LoadMigrations reads and validates the embedded migration set.
func LoadMigrations() ([]Migration, error) {
	return loadMigrations(migrationsFS)
}

func loadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.Glob(fsys, path.Join(migrationsDir, "*.sql"))
	if err != nil {
		return nil, fmt.Errorf("store: list migrations: %w", err)
	}

	migrations := make([]Migration, 0, len(entries))
	seen := map[int]string{}
	for _, file := range entries {
		base := path.Base(file)
		version, name, err := parseMigrationName(base)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("store: migration version %d is claimed by both %s and %s",
				version, prev, base)
		}
		seen[version] = base

		body, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, fmt.Errorf("store: read %s: %w", file, err)
		}
		migrations = append(migrations, Migration{
			Version: version,
			Name:    name,
			File:    base,
			SQL:     string(body),
		})
	}
	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})
	if len(migrations) == 0 {
		return nil, errors.New("store: no migrations are embedded")
	}
	return migrations, nil
}

// parseMigrationName splits NNNN_name.sql into its version and name.
func parseMigrationName(base string) (int, string, error) {
	stem := strings.TrimSuffix(base, ".sql")
	digits, name, found := strings.Cut(stem, "_")
	if !found || digits == "" || name == "" {
		return 0, "", fmt.Errorf("store: migration %q is not named NNNN_name.sql", base)
	}
	version, err := strconv.Atoi(digits)
	if err != nil || version <= 0 {
		return 0, "", fmt.Errorf("store: migration %q has no positive numeric prefix", base)
	}
	return version, name, nil
}

const createSchemaMigrations = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL
)`

// Migrate applies every embedded migration the database has not yet recorded
// and returns the versions applied by this call.
//
// Each migration runs in its own transaction together with the row recording
// it, so a migration and the claim that it ran can never disagree. A database
// recording versions this binary does not carry is refused outright rather
// than partially interpreted.
func (d *DB) Migrate(ctx context.Context) ([]int, error) {
	migrations, err := LoadMigrations()
	if err != nil {
		return nil, err
	}
	return migrate(ctx, d.sql, migrations)
}

func migrate(ctx context.Context, sqldb *sql.DB, migrations []Migration) ([]int, error) {
	if _, err := sqldb.ExecContext(ctx, createSchemaMigrations); err != nil {
		return nil, fmt.Errorf("store: create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, sqldb)
	if err != nil {
		return nil, err
	}

	known := map[int]Migration{}
	newest := 0
	for _, m := range migrations {
		known[m.Version] = m
		if m.Version > newest {
			newest = m.Version
		}
	}
	for _, version := range applied {
		if _, ok := known[version]; ok {
			continue
		}
		if version > newest {
			return nil, fmt.Errorf("%w: database has migration %d, newest known is %d",
				ErrSchemaAhead, version, newest)
		}
		return nil, fmt.Errorf("%w: version %d is recorded but not embedded",
			ErrSchemaUnknown, version)
	}

	appliedSet := map[int]bool{}
	for _, version := range applied {
		appliedSet[version] = true
	}

	var ran []int
	for _, m := range migrations {
		if appliedSet[m.Version] {
			continue
		}
		if err := applyMigration(ctx, sqldb, m); err != nil {
			return ran, err
		}
		ran = append(ran, m.Version)
	}
	return ran, nil
}

func appliedVersions(ctx context.Context, sqldb *sql.DB) ([]int, error) {
	rows, err := sqldb.QueryContext(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: read schema_migrations: %w", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	return versions, nil
}

func applyMigration(ctx context.Context, sqldb *sql.DB, m Migration) error {
	tx, err := sqldb.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %d: %w", m.Version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("store: apply migration %s: %w", m.File, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)",
		m.Version, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("store: record migration %d: %w", m.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %d: %w", m.Version, err)
	}
	return nil
}
