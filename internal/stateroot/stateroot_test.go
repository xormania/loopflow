package stateroot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultRootFollowsXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/somewhere/state")
	got, err := DefaultRoot()
	if err != nil {
		t.Fatalf("DefaultRoot: %v", err)
	}
	if got != "/somewhere/state/wfc" {
		t.Errorf("DefaultRoot = %q, want /somewhere/state/wfc", got)
	}

	t.Setenv("XDG_STATE_HOME", "")
	got, err = DefaultRoot()
	if err != nil {
		t.Fatalf("DefaultRoot: %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join(".local", "state", "wfc")) {
		t.Errorf("DefaultRoot = %q, want a ~/.local/state/wfc path", got)
	}
}

func TestNewResolvesPathsUnderTheRoot(t *testing.T) {
	l, err := New("/var/lib/wfc-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for name, path := range map[string]string{"Database": l.Database, "Artifacts": l.Artifacts} {
		if !strings.HasPrefix(path, l.Root+string(os.PathSeparator)) {
			t.Errorf("%s = %q, want a path under %q", name, path, l.Root)
		}
	}
}

func TestEnsureCreatesPrivateDirectories(t *testing.T) {
	l, err := New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := l.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, dir := range []string{l.Root, l.Artifacts} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != DirMode {
			t.Errorf("%s mode = %04o, want %04o", dir, perm, DirMode)
		}
	}
	if err := l.Ensure(); err != nil {
		t.Fatalf("Ensure is not idempotent: %v", err)
	}
}
