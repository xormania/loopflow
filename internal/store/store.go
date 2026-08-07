// Package store owns the control-plane SQLite database: opening it with the
// required pragmas, applying embedded migrations, and the sqlc-generated
// query layer.
//
// Exactly one process — flowd — opens this database (decisions.md D2). The
// driver is modernc.org/sqlite, the only runtime dependency permitted by D4.
package store

import (
	"embed"

	// Registers the pure-Go "sqlite" driver. No cgo (decisions.md D4).
	_ "modernc.org/sqlite"
)

// DriverName is the database/sql driver registered by modernc.org/sqlite.
const DriverName = "sqlite"

// migrationsFS holds the numbered schema migrations. They are embedded so a
// deployed binary carries its own schema and cannot be applied against files
// that drifted from the build.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations exposes the embedded migration files to the migration runner
// implemented in Phase 1.
func Migrations() embed.FS { return migrationsFS }
