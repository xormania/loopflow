package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// BusyTimeoutMS is the SQLite busy timeout applied to every connection.
const BusyTimeoutMS = 5000

// DirMode is the mode of directories created under the state root. The
// filesystem is the control plane's only access control (decisions.md D3), so
// nothing it creates is group- or world-readable.
const DirMode os.FileMode = 0o700

// DB is a handle on the control-plane database. It embeds the generated
// *Queries, so every query is available directly on it for work that needs no
// transaction; use Tx for work that does.
type DB struct {
	*Queries

	sql  *sql.DB
	path string
}

// Open opens (creating if absent) the database at path and verifies that the
// required pragmas actually took effect.
//
// Verification is not ceremony: the pragmas travel as driver-specific DSN
// parameters, so a driver that silently ignored one would leave foreign keys
// off or the busy timeout at zero, and the failure would surface much later as
// corrupt references or spurious lock errors. Fail closed at open instead.
//
// Open does not migrate. Callers run Migrate explicitly so that startup
// ordering is visible at the call site.
func Open(ctx context.Context, path string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve %q: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), DirMode); err != nil {
		return nil, fmt.Errorf("store: create state directory: %w", err)
	}

	sqldb, err := sql.Open(DriverName, dsn(abs))
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", abs, err)
	}
	if err := sqldb.PingContext(ctx); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("store: open %q: %w", abs, err)
	}
	if err := verifyPragmas(ctx, sqldb); err != nil {
		_ = sqldb.Close()
		return nil, err
	}

	return &DB{Queries: New(sqldb), sql: sqldb, path: abs}, nil
}

// dsn builds the driver DSN. The path is carried as a file URL so that a state
// root containing characters significant to the query string cannot corrupt
// the parameters.
func dsn(absPath string) string {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", BusyTimeoutMS))
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(on)")
	// Every transaction takes the write lock at BEGIN. Deferred transactions
	// that upgrade to a write mid-way fail with SQLITE_BUSY_SNAPSHOT, which
	// the busy timeout does not retry; taking the lock up front turns that
	// unrecoverable case into ordinary, bounded contention.
	q.Set("_txlock", "immediate")

	u := url.URL{Scheme: "file", Path: absPath, RawQuery: q.Encode()}
	return u.String()
}

func verifyPragmas(ctx context.Context, sqldb *sql.DB) error {
	var journal string
	if err := sqldb.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		return fmt.Errorf("store: read journal_mode: %w", err)
	}
	if !strings.EqualFold(journal, "wal") {
		return fmt.Errorf("store: journal_mode is %q, want wal", journal)
	}

	var foreignKeys int
	if err := sqldb.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return fmt.Errorf("store: read foreign_keys: %w", err)
	}
	if foreignKeys != 1 {
		return fmt.Errorf("store: foreign_keys is %d, want 1", foreignKeys)
	}

	var busyTimeout int
	if err := sqldb.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		return fmt.Errorf("store: read busy_timeout: %w", err)
	}
	if busyTimeout != BusyTimeoutMS {
		return fmt.Errorf("store: busy_timeout is %d, want %d", busyTimeout, BusyTimeoutMS)
	}
	return nil
}

// Close releases the database handle.
func (d *DB) Close() error { return d.sql.Close() }

// Path is the absolute path of the database file.
func (d *DB) Path() string { return d.path }

// SQL exposes the underlying handle for work the generated queries do not
// cover, such as migrations.
func (d *DB) SQL() *sql.DB { return d.sql }

// Tx runs fn inside a transaction, committing when fn returns nil and rolling
// back otherwise. The *Queries passed to fn is bound to the transaction; using
// the outer DB inside fn would silently escape it.
func (d *DB) Tx(ctx context.Context, fn func(*Queries) error) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(d.Queries.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}
