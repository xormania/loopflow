// Package stateroot resolves where wfc keeps its data.
//
// Everything lives under $XDG_STATE_HOME/wfc, defaulting to
// ~/.local/state/wfc (decisions.md D12): the SQLite database and the
// content-addressed artifact store. Directories are created 0700 — the
// artifact store holds evidence, and filesystem permissions are the only thing
// protecting it.
package stateroot

import (
	"fmt"
	"os"
	"path/filepath"
)

// DirMode is the mode of every directory in the layout.
const DirMode os.FileMode = 0o700

// Layout is the resolved set of paths.
type Layout struct {
	// Root is the state root directory.
	Root string
	// Database is the SQLite file.
	Database string
	// Artifacts is the content-addressed store root.
	Artifacts string
}

// DefaultRoot is $XDG_STATE_HOME/wfc, or ~/.local/state/wfc when that is unset.
func DefaultRoot() (string, error) {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "wfc"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("stateroot: no XDG_STATE_HOME and no home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "wfc"), nil
}

// New resolves the layout for a state root without creating anything.
func New(root string) (Layout, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, fmt.Errorf("stateroot: resolve %q: %w", root, err)
	}
	if err := refuseInsideRepository(abs); err != nil {
		return Layout{}, err
	}
	return Layout{
		Root:      abs,
		Database:  filepath.Join(abs, "control.sqlite"),
		Artifacts: filepath.Join(abs, "artifacts"),
	}, nil
}

// refuseInsideRepository rejects a state root under a Git work tree.
//
// A tool that writes inside a checkout can invalidate packet manifest custody
// — a stray write to .claude/settings.local.json once cost an otherwise
// successful RED. "wfc writes nothing into repositories" was previously true
// only of the default configuration; -root and WFC_ROOT could still be pointed
// at one. This makes it an enforced property instead of a habit.
func refuseInsideRepository(root string) error {
	for dir := root; ; {
		if isRepository(dir) {
			return fmt.Errorf("stateroot: %s is inside the Git work tree at %s; "+
				"wfc must not write into a checkout, because that can invalidate "+
				"packet manifest custody", root, dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// isRepository reports whether dir is the root of a Git work tree.
//
// The presence of something named .git is not enough — an empty directory of
// that name exists on this host and is not a repository. This sniffs the way
// Git does: a file is a worktree or submodule pointer, and a directory has to
// contain a HEAD.
func isRepository(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	if !info.IsDir() {
		return true
	}
	_, err = os.Stat(filepath.Join(dir, ".git", "HEAD"))
	return err == nil
}

// Default resolves the layout for the default state root.
func Default() (Layout, error) {
	root, err := DefaultRoot()
	if err != nil {
		return Layout{}, err
	}
	return New(root)
}

// Ensure creates the directories in the layout, each 0700.
func (l Layout) Ensure() error {
	for _, dir := range []string{l.Root, l.Artifacts} {
		if err := os.MkdirAll(dir, DirMode); err != nil {
			return fmt.Errorf("stateroot: create %s: %w", dir, err)
		}
	}
	return nil
}
