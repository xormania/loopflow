package stateroot

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// aProject is a stand-in for tests that need any valid project.
var aProject = Project{Key: DefaultProjectKey, Name: "default"}

func writeFakeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	return dir
}

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

func TestNewResolvesPathsUnderTheProject(t *testing.T) {
	l, err := New("/var/lib/loopflow-test", aProject)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	prefix := l.ProjectDir + string(os.PathSeparator)
	for name, path := range map[string]string{"Database": l.Database, "Artifacts": l.Artifacts} {
		if !strings.HasPrefix(path, prefix) {
			t.Errorf("%s = %q, want a path under %q", name, path, l.ProjectDir)
		}
	}
	if !strings.HasPrefix(l.ProjectDir, filepath.Join(l.Root, "projects")+string(os.PathSeparator)) {
		t.Errorf("ProjectDir = %q, want a path under %q", l.ProjectDir, filepath.Join(l.Root, "projects"))
	}
}

// Two projects under one root must never share a database or an artifact
// store — that separation is the whole point of projects.
func TestDifferentProjectsNeverSharePaths(t *testing.T) {
	a, err := New("/var/lib/loopflow-test", Project{Key: "aaaa", Name: "a"})
	if err != nil {
		t.Fatalf("New a: %v", err)
	}
	b, err := New("/var/lib/loopflow-test", Project{Key: "bbbb", Name: "b"})
	if err != nil {
		t.Fatalf("New b: %v", err)
	}
	if a.Database == b.Database || a.Artifacts == b.Artifacts {
		t.Errorf("projects share paths: %q vs %q", a.Database, b.Database)
	}
}

func TestNewRefusesAnEmptyProject(t *testing.T) {
	if _, err := New("/var/lib/loopflow-test", Project{}); err == nil {
		t.Error("New accepted a layout without a project")
	}
}

func TestEnsureCreatesPrivateDirectoriesAndTheMarker(t *testing.T) {
	l, err := New(filepath.Join(t.TempDir(), "state"), Project{Key: "abc123", Name: "demo", Source: "github.com/o/demo"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := l.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, dir := range []string{l.Root, l.ProjectDir, l.Artifacts} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != DirMode {
			t.Errorf("%s mode = %04o, want %04o", dir, perm, DirMode)
		}
	}
	name, source, err := ReadMarker(l.ProjectDir)
	if err != nil {
		t.Fatalf("ReadMarker: %v", err)
	}
	if name != "demo" || source != "github.com/o/demo" {
		t.Errorf("marker = %q %q, want demo github.com/o/demo", name, source)
	}
	if _, err := l.Ensure(); err != nil {
		t.Fatalf("Ensure is not idempotent: %v", err)
	}
}

// A pre-project root kept its database and artifacts directly under the root.
// That state is somebody's record; Ensure adopts it into the default project
// instead of stranding it where no command will look again.
func TestEnsureAdoptsLegacyState(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"control.sqlite", "control.sqlite-wal"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte(f), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "artifacts", "sha256"), 0o700); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}

	l, err := New(root, Project{Key: "abc123", Name: "demo"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	adopted, err := l.Ensure()
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !adopted {
		t.Error("Ensure did not report adopting legacy state")
	}
	def := filepath.Join(root, "projects", DefaultProjectKey)
	for _, f := range []string{"control.sqlite", "control.sqlite-wal"} {
		if _, err := os.Stat(filepath.Join(def, f)); err != nil {
			t.Errorf("legacy %s not adopted: %v", f, err)
		}
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			t.Errorf("legacy %s still at the root", f)
		}
	}
	if _, err := os.Stat(filepath.Join(def, "artifacts", "sha256")); err != nil {
		t.Errorf("legacy artifacts not adopted: %v", err)
	}

	adopted, err = l.Ensure()
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if adopted {
		t.Error("second Ensure claims to adopt again")
	}
}

// Two databases cannot be merged automatically; refusing loudly beats
// silently shadowing one of them.
func TestEnsureRefusesConflictingLegacyState(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "control.sqlite"), []byte("legacy"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	def := filepath.Join(root, "projects", DefaultProjectKey)
	if err := os.MkdirAll(def, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(def, "control.sqlite"), []byte("current"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	l, err := New(root, aProject)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := l.Ensure(); err == nil {
		t.Error("Ensure merged two databases silently")
	}
}

// A state root inside a checkout can invalidate packet manifest custody, so it
// is refused rather than merely discouraged.
func TestNewRefusesAStateRootInsideARepository(t *testing.T) {
	repo := writeFakeRepo(t)
	if _, err := New(filepath.Join(repo, "proj", "state"), aProject); err == nil {
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
	if _, err := New(filepath.Join(dir, "state"), aProject); err != nil {
		t.Errorf("New refused a path under an empty .git directory: %v", err)
	}
}

func TestDeriveProjectOutsideARepositoryIsTheDefault(t *testing.T) {
	p, err := DeriveProject(t.TempDir())
	if err != nil {
		t.Fatalf("DeriveProject: %v", err)
	}
	if p.Key != DefaultProjectKey {
		t.Errorf("Key = %q, want %q", p.Key, DefaultProjectKey)
	}
}

// Without a remote, identity falls back to the tree's path — and every
// directory inside the tree resolves to the same project.
func TestDeriveProjectUsesThePathWhenThereIsNoRemote(t *testing.T) {
	repo := writeFakeRepo(t)
	nested := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	top, err := DeriveProject(repo)
	if err != nil {
		t.Fatalf("DeriveProject(top): %v", err)
	}
	inner, err := DeriveProject(nested)
	if err != nil {
		t.Fatalf("DeriveProject(nested): %v", err)
	}
	if top.Key != inner.Key {
		t.Errorf("keys differ inside one tree: %q vs %q", top.Key, inner.Key)
	}
	if top.Key == DefaultProjectKey || len(top.Key) != 12 {
		t.Errorf("Key = %q, want a 12-hex derived key", top.Key)
	}
	if top.Name != filepath.Base(top.Source) {
		t.Errorf("Name = %q, want the tree's basename %q", top.Name, filepath.Base(top.Source))
	}
}

// The same repository cloned into two different directories — two containers,
// two mount points — must be one project. Identity is the origin remote, so
// the paths must not matter.
func TestDeriveProjectIsTheSameAcrossClonesOfOneRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
	mkClone := func() string {
		dir := t.TempDir()
		for _, args := range [][]string{
			{"init", "-q"},
			{"remote", "add", "origin", "git@example.com:owner/repo.git"},
		} {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		return dir
	}

	a, err := DeriveProject(mkClone())
	if err != nil {
		t.Fatalf("DeriveProject(a): %v", err)
	}
	b, err := DeriveProject(mkClone())
	if err != nil {
		t.Fatalf("DeriveProject(b): %v", err)
	}
	if a.Key != b.Key {
		t.Errorf("one remote, two keys: %q vs %q", a.Key, b.Key)
	}
	if a.Source != "example.com/owner/repo" {
		t.Errorf("Source = %q, want example.com/owner/repo", a.Source)
	}
	if a.Name != "repo" {
		t.Errorf("Name = %q, want repo", a.Name)
	}
}

// Every way of spelling one repository's URL is one identity.
func TestNormalizeRemote(t *testing.T) {
	want := "github.com/Owner/Repo"
	for _, url := range []string{
		"https://github.com/Owner/Repo.git",
		"https://github.com/Owner/Repo",
		"http://github.com/Owner/Repo.git",
		"git@github.com:Owner/Repo.git",
		"ssh://git@github.com/Owner/Repo.git",
		"git://GitHub.com/Owner/Repo/",
		"https://user@github.com/Owner/Repo.git",
	} {
		if got := NormalizeRemote(url); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestNamedProject(t *testing.T) {
	if p, err := NamedProject("loopstrap"); err != nil {
		t.Errorf("NamedProject(loopstrap): %v", err)
	} else if p.Key != "loopstrap" || p.Name != "loopstrap" {
		t.Errorf("NamedProject(loopstrap) = %+v", p)
	}

	for _, bad := range []string{"", "_default", ".hidden", "a b", "a/b", strings.Repeat("a", 65)} {
		if _, err := NamedProject(bad); err == nil {
			t.Errorf("NamedProject(%q) was accepted", bad)
		}
	}
}
