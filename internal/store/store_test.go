package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "state", "control.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenAppliesPragmas(t *testing.T) {
	db := openTestDB(t)

	// verifyPragmas already ran inside Open; re-assert here so a regression
	// names the pragma rather than only failing to open.
	var journal string
	if err := db.SQL().QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if !strings.EqualFold(journal, "wal") {
		t.Errorf("journal_mode = %q, want wal", journal)
	}

	var fk, busy int
	if err := db.SQL().QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
	if err := db.SQL().QueryRowContext(t.Context(), "PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if busy != BusyTimeoutMS {
		t.Errorf("busy_timeout = %d, want %d", busy, BusyTimeoutMS)
	}
}

func TestOpenCreatesStateDirectoryPrivately(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "state")
	db, err := Open(t.Context(), filepath.Join(dir, "control.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != DirMode {
		t.Errorf("state directory mode = %04o, want %04o", perm, DirMode)
	}
	if !filepath.IsAbs(db.Path()) {
		t.Errorf("Path() = %q, want an absolute path", db.Path())
	}
}

func TestMigrateAppliesEverythingOnceAndIsIdempotent(t *testing.T) {
	db := openTestDB(t)

	embedded, err := LoadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(embedded) == 0 {
		t.Fatal("no embedded migrations")
	}

	applied, err := db.Migrate(t.Context())
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(applied) != len(embedded) {
		t.Fatalf("applied %v, want %d migrations", applied, len(embedded))
	}

	// Every table the foundation migration declares now exists.
	for _, table := range []string{"packets", "events", "packet_state", "artifacts", "schema_migrations"} {
		var name string
		err := db.SQL().QueryRowContext(t.Context(),
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing after migrate: %v", table, err)
		}
	}

	again, err := db.Migrate(t.Context())
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second migrate applied %v, want nothing", again)
	}
}

func TestMigrateRefusesSchemaAhead(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Simulate a database written by a newer binary.
	if _, err := db.SQL().ExecContext(t.Context(),
		"INSERT INTO schema_migrations (version, applied_at) VALUES (9999, '2026-01-01T00:00:00Z')"); err != nil {
		t.Fatalf("seed future migration: %v", err)
	}

	_, err := db.Migrate(t.Context())
	if !errors.Is(err, ErrSchemaAhead) {
		t.Fatalf("migrate error = %v, want ErrSchemaAhead", err)
	}
}

func TestMigrateRefusesUnknownVersion(t *testing.T) {
	db := openTestDB(t)
	set := []Migration{
		{Version: 1, Name: "one", File: "0001_one.sql", SQL: "CREATE TABLE one (a TEXT);"},
		{Version: 3, Name: "three", File: "0003_three.sql", SQL: "CREATE TABLE three (a TEXT);"},
	}
	if _, err := migrate(t.Context(), db.SQL(), set); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Version 2 was never embedded, but the database claims it ran: the two
	// migration sets have diverged.
	if _, err := db.SQL().ExecContext(t.Context(),
		"INSERT INTO schema_migrations (version, applied_at) VALUES (2, '2026-01-01T00:00:00Z')"); err != nil {
		t.Fatalf("seed divergent migration: %v", err)
	}

	_, err := migrate(t.Context(), db.SQL(), set)
	if !errors.Is(err, ErrSchemaUnknown) {
		t.Fatalf("migrate error = %v, want ErrSchemaUnknown", err)
	}
}

func TestMigrateRollsBackFailedMigration(t *testing.T) {
	db := openTestDB(t)
	set := []Migration{
		{Version: 1, Name: "ok", File: "0001_ok.sql", SQL: "CREATE TABLE ok (a TEXT);"},
		{Version: 2, Name: "broken", File: "0002_broken.sql", SQL: "CREATE TABLE broken (a TEXT); NOT SQL;"},
	}
	ran, err := migrate(t.Context(), db.SQL(), set)
	if err == nil {
		t.Fatal("migrate succeeded on a broken migration, want error")
	}
	if len(ran) != 1 || ran[0] != 1 {
		t.Errorf("applied %v, want only version 1", ran)
	}

	// The broken migration left nothing behind: neither its table nor a row
	// claiming it ran.
	var name string
	err = db.SQL().QueryRowContext(t.Context(),
		"SELECT name FROM sqlite_master WHERE type='table' AND name='broken'").Scan(&name)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("table from failed migration exists (err=%v)", err)
	}
	var version int
	err = db.SQL().QueryRowContext(t.Context(),
		"SELECT version FROM schema_migrations WHERE version=2").Scan(&version)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("failed migration was recorded as applied (err=%v)", err)
	}
}

func TestLoadMigrationsRejectsMalformedNames(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"no numeric prefix": {"migrations/foundation.sql": &fstest.MapFile{Data: []byte("")}},
		"empty name":        {"migrations/0001_.sql": &fstest.MapFile{Data: []byte("")}},
		"zero version":      {"migrations/0000_zero.sql": &fstest.MapFile{Data: []byte("")}},
		"non-numeric":       {"migrations/abc_x.sql": &fstest.MapFile{Data: []byte("")}},
		"duplicate version": {
			"migrations/0001_a.sql": &fstest.MapFile{Data: []byte("")},
			"migrations/001_b.sql":  &fstest.MapFile{Data: []byte("")},
		},
		"empty set": {"other/0001_a.sql": &fstest.MapFile{Data: []byte("")}},
	}
	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadMigrations(fsys); err == nil {
				t.Error("loadMigrations succeeded, want error")
			}
		})
	}
}

func TestLoadMigrationsOrdersByVersion(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/0010_ten.sql": &fstest.MapFile{Data: []byte("-- ten")},
		"migrations/0002_two.sql": &fstest.MapFile{Data: []byte("-- two")},
		"migrations/0001_one.sql": &fstest.MapFile{Data: []byte("-- one")},
	}
	got, err := loadMigrations(fsys)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	want := []int{1, 2, 10}
	if len(got) != len(want) {
		t.Fatalf("got %d migrations, want %d", len(got), len(want))
	}
	for i, m := range got {
		if m.Version != want[i] {
			t.Errorf("migration %d has version %d, want %d", i, m.Version, want[i])
		}
	}
	if got[0].Name != "one" || got[0].File != "0001_one.sql" || got[0].SQL != "-- one" {
		t.Errorf("unexpected first migration: %+v", got[0])
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	err := db.InsertEvent(t.Context(), InsertEventParams{
		PacketID:    "nonexistent",
		Seq:         1,
		Hash:        strings.Repeat("a", 64),
		Prev:        strings.Repeat("0", 64),
		Time:        "2026-01-01T00:00:00Z",
		Payload:     "{}",
		StateSha256: strings.Repeat("b", 64),
	})
	if err == nil {
		t.Fatal("inserted an event for a packet that does not exist")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Errorf("error = %v, want a foreign key violation", err)
	}
}

func TestTxRollsBackOnError(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	sentinel := errors.New("sentinel")
	err := db.Tx(t.Context(), func(q *Queries) error {
		if err := q.CreatePacket(t.Context(), CreatePacketParams{
			PacketID: "p1", CreatedAt: "2026-01-01T00:00:00Z",
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Tx error = %v, want sentinel", err)
	}

	if _, err := db.GetPacket(t.Context(), "p1"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("packet survived a rolled-back transaction (err=%v)", err)
	}
}

func TestTxCommits(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	err := db.Tx(t.Context(), func(q *Queries) error {
		return q.CreatePacket(t.Context(), CreatePacketParams{
			PacketID: "p1", CreatedAt: "2026-01-01T00:00:00Z",
		})
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if _, err := db.GetPacket(t.Context(), "p1"); err != nil {
		t.Errorf("packet missing after commit: %v", err)
	}
}

// Several wfc processes routinely start at once, and on a fresh database they
// all try to migrate. Before the check moved inside the write lock, they raced:
// two would read "nothing applied" and both try to create the same table.
func TestConcurrentMigrateIsSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "control.sqlite")

	const racers = 8
	errs := make(chan error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		go func() {
			<-start
			db, err := Open(t.Context(), path)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = db.Close() }()
			_, err = db.Migrate(t.Context())
			errs <- err
		}()
	}
	close(start)

	for i := 0; i < racers; i++ {
		if err := <-errs; err != nil {
			t.Errorf("racer %d: %v", i, err)
		}
	}

	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Each migration ran exactly once.
	embedded, err := LoadMigrations()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var applied int
	if err := db.SQL().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM schema_migrations").Scan(&applied); err != nil {
		t.Fatalf("count: %v", err)
	}
	if applied != len(embedded) {
		t.Errorf("schema_migrations has %d rows, want %d", applied, len(embedded))
	}
}
