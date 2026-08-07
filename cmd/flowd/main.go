// Command flowd is the workflow control-plane server.
//
// It is the sole owner of the control-plane SQLite database (decisions.md D2)
// and will serve HTTP/1.1 + JSON over a unix domain socket (D3). Phase 1 gives
// it only what the foundation supports: it opens the database, applies the
// embedded migrations, and exits. The listener, single-instance lock, and
// graceful shutdown arrive in Phase 2, which also owns the real state-root
// layout that defaultDatabasePath sketches here.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/xormania/wfc/internal/store"
)

// version is the build identity reported by both binaries. Phase 2 replaces
// this with a real version/health endpoint.
const version = "0.0.0-dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "flowd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dbPath := flag.String("db", defaultDatabasePath(), "control-plane database path")
	flag.Parse()

	ctx := context.Background()

	db, err := store.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	applied, err := db.Migrate(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("flowd %s\n", version)
	fmt.Printf("database: %s\n", db.Path())
	if len(applied) == 0 {
		fmt.Println("schema: up to date")
	} else {
		fmt.Printf("schema: applied migrations %v\n", applied)
	}
	return nil
}

// defaultDatabasePath resolves $XDG_STATE_HOME/wfc/control.sqlite, defaulting
// to ~/.local/state/wfc/control.sqlite (decisions.md D12). The state root
// deliberately avoids the collisions recorded in environment-facts.md 4b: it
// is never inside a repository proj/ tree, and never under ~/.agent-lab.
func defaultDatabasePath() string {
	root := os.Getenv("XDG_STATE_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// Fail closed onto a relative path rather than guessing at a
			// system-wide location; the operator can always pass -db.
			return filepath.Join(".wfc", "control.sqlite")
		}
		root = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(root, "wfc", "control.sqlite")
}
