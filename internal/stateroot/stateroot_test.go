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
	if got != "/somewhere/state/loopflow" {
		t.Errorf("DefaultRoot = %q, want /somewhere/state/loopflow", got)
	}

	t.Setenv("XDG_STATE_HOME", "")
	got, err = DefaultRoot()
	if err != nil {
		t.Fatalf("DefaultRoot: %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join(".local", "state", "loopflow")) {
		t.Errorf("DefaultRoot = %q, want a ~/.local/state/loopflow path", got)
	}
}

func TestNewResolvesPathsUnderTheRoot(t *testing.T) {
	l, err := New("/var/lib/loopflow-test")
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

// A state root inside a checkout can invalidate packet manifest custody, so it
// is refused rather than merely discouraged.
func TestNewRefusesAStateRootInsideARepository(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}

	if _, err := New(filepath.Join(repo, "proj", "state")); err == nil {
		t.Error("New accepted a state root inside a Git work tree")
	} else if !strings.Contains(err.Error(), "work tree") {
		t.Errorf("error = %v, want it to name the work tree", err)
	}
}

// A directory merely named .git is not a repository. One exists on this host
// and is empty; treating it as a checkout would block ordinary temp paths.
func TestNewAcceptsADirectoryNamedGitThatIsNotARepository(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := New(filepath.Join(dir, "state")); err != nil {
		t.Errorf("New refused a path under an empty .git directory: %v", err)
	}
}
