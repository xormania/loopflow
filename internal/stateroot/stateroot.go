// Package stateroot resolves where loopflow keeps its data.
//
// Everything lives under $XDG_STATE_HOME/loopflow, defaulting to
// ~/.local/state/loopflow (decisions.md D12), and within that root each
// project has its own slice: projects/<key>/ holds the SQLite database and
// the content-addressed artifact store for one project, so packets in
// different repositories can never collide. Directories are created 0700 —
// the artifact store holds evidence, and filesystem permissions are the only
// thing protecting it.
package stateroot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// DirMode is the mode of every directory in the layout.
const DirMode os.FileMode = 0o700

// Layout is the resolved set of paths.
type Layout struct {
	// Root is the state root directory.
	Root string
	// Project is the project this layout is scoped to.
	Project Project
	// ProjectDir is the project's slice of the root.
	ProjectDir string
	// Database is the SQLite file.
	Database string
	// Artifacts is the content-addressed store root.
	Artifacts string
}

// Project identifies which project's slice of the state root to use.
type Project struct {
	// Key is the directory name under projects/.
	Key string
	// Name is the human label shown in listings.
	Name string
	// Source is what the key was derived from — a normalized remote URL or a
	// work tree path; empty for a named project and for the default.
	Source string
}

// DefaultProjectKey is the reserved project used outside any git work tree.
// The leading underscore keeps it out of the namespace NamedProject accepts.
const DefaultProjectKey = "_default"

// DeriveProject finds the git work tree enclosing dir and identifies its
// project. Identity comes from the repository's origin remote when it has
// one: the same repo cloned into different containers, or mounted at
// different paths, must resolve to the same project, or parallel harnesses
// would silently fork its state. Only a repo with no remote falls back to
// its resolved path. Outside any work tree the reserved default project is
// returned rather than an error — loose use is allowed, just corralled.
func DeriveProject(dir string) (Project, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Project{}, fmt.Errorf("stateroot: resolve %q: %w", dir, err)
	}
	// Symlinked and direct paths to one work tree must be one project too.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	for d := abs; ; {
		if isRepository(d) {
			if remote := originRemote(d); remote != "" {
				return Project{
					Key:    deriveKey("remote", remote),
					Name:   path.Base(remote),
					Source: remote,
				}, nil
			}
			return Project{
				Key:    deriveKey("path", d),
				Name:   filepath.Base(d),
				Source: d,
			}, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return Project{Key: DefaultProjectKey, Name: "default"}, nil
		}
		d = parent
	}
}

// deriveKey hashes an identity into a directory name. The namespace prefix
// keeps a path that happens to spell a URL from colliding with the URL.
func deriveKey(namespace, identity string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + identity))
	return hex.EncodeToString(sum[:])[:12]
}

// originRemote returns the normalized origin URL of the work tree at top, or
// "" when there is no origin or no git binary to ask.
func originRemote(top string) string {
	out, err := exec.Command("git", "-C", top, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return NormalizeRemote(strings.TrimSpace(string(out)))
}

// NormalizeRemote reduces the ways one repository is addressed to one form:
// host/owner/repo, lowercase host, no scheme, no credentials, no .git. The
// clone URL is identity here, so https://, ssh://, and scp-style spellings
// of the same repository must not become three projects.
func NormalizeRemote(url string) string {
	s := strings.TrimSpace(url)
	for _, scheme := range []string{"https://", "http://", "ssh://", "git://"} {
		if strings.HasPrefix(s, scheme) {
			s = strings.TrimPrefix(s, scheme)
			break
		}
	}
	if at := strings.Index(s, "@"); at >= 0 && !strings.Contains(s[:at], "/") {
		s = s[at+1:] // user@ or git@ credential prefix
	}
	// scp-style host:owner/repo — the colon plays the role of the first slash.
	if colon := strings.Index(s, ":"); colon >= 0 && !strings.Contains(s[:colon], "/") {
		s = s[:colon] + "/" + s[colon+1:]
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	if slash := strings.Index(s, "/"); slash > 0 {
		s = strings.ToLower(s[:slash]) + s[slash:]
	}
	return s
}

// namedProject is what -project accepts: something usable as a directory name
// on any filesystem, not starting with the underscore that marks reserved
// keys or a dot that would hide it.
var namedProject = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// NamedProject validates an explicitly chosen project name.
func NamedProject(name string) (Project, error) {
	if !namedProject.MatchString(name) {
		return Project{}, fmt.Errorf("stateroot: project name %q must start with a letter or digit "+
			"and contain only letters, digits, '.', '_' and '-' (max 64)", name)
	}
	return Project{Key: name, Name: name}, nil
}

// DefaultRoot is $XDG_STATE_HOME/loopflow, or ~/.local/state/loopflow when that is unset.
func DefaultRoot() (string, error) {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "loopflow"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("stateroot: no XDG_STATE_HOME and no home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "loopflow"), nil
}

// New resolves the layout for a project under a state root without creating
// anything.
func New(root string, p Project) (Layout, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, fmt.Errorf("stateroot: resolve %q: %w", root, err)
	}
	if err := refuseInsideRepository(abs); err != nil {
		return Layout{}, err
	}
	if p.Key == "" {
		return Layout{}, fmt.Errorf("stateroot: layout needs a project")
	}
	dir := filepath.Join(abs, "projects", p.Key)
	return Layout{
		Root:       abs,
		Project:    p,
		ProjectDir: dir,
		Database:   filepath.Join(dir, "control.sqlite"),
		Artifacts:  filepath.Join(dir, "artifacts"),
	}, nil
}

// refuseInsideRepository rejects a state root under a Git work tree.
//
// A tool that writes inside a checkout can invalidate packet manifest custody
// — a stray write to .claude/settings.local.json once cost an otherwise
// successful RED. "loopflow writes nothing into repositories" was previously true
// only of the default configuration; -root and LOOPFLOW_ROOT could still be pointed
// at one. This makes it an enforced property instead of a habit.
func refuseInsideRepository(root string) error {
	for dir := root; ; {
		if isRepository(dir) {
			return fmt.Errorf("stateroot: %s is inside the Git work tree at %s; "+
				"loopflow must not write into a checkout, because that can invalidate "+
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

// Default resolves the layout for a project under the default state root.
func Default(p Project) (Layout, error) {
	root, err := DefaultRoot()
	if err != nil {
		return Layout{}, err
	}
	return New(root, p)
}

// Ensure creates the directories in the layout, each 0700, and records the
// project marker. It reports whether pre-project state was adopted into the
// default project on the way.
func (l Layout) Ensure() (adoptedLegacy bool, err error) {
	adoptedLegacy, err = l.adoptLegacy()
	if err != nil {
		return false, err
	}
	for _, dir := range []string{l.Root, l.ProjectDir, l.Artifacts} {
		if err := os.MkdirAll(dir, DirMode); err != nil {
			return adoptedLegacy, fmt.Errorf("stateroot: create %s: %w", dir, err)
		}
	}
	if err := l.writeMarker(); err != nil {
		return adoptedLegacy, err
	}
	return adoptedLegacy, nil
}

// adoptLegacy moves a pre-project database and artifact store from the root
// itself into the reserved default project. The state is somebody's record;
// a layout change is not a reason to lose it or to leave it stranded where
// no command will look again.
func (l Layout) adoptLegacy() (bool, error) {
	legacyDB := filepath.Join(l.Root, "control.sqlite")
	if _, err := os.Stat(legacyDB); err != nil {
		return false, nil
	}
	target := filepath.Join(l.Root, "projects", DefaultProjectKey)
	if _, err := os.Stat(filepath.Join(target, "control.sqlite")); err == nil {
		return false, fmt.Errorf("stateroot: both %s and %s exist; two databases cannot be merged "+
			"automatically — move one aside", legacyDB, filepath.Join(target, "control.sqlite"))
	}
	if err := os.MkdirAll(target, DirMode); err != nil {
		return false, fmt.Errorf("stateroot: create %s: %w", target, err)
	}
	// The database and any SQLite sidecars move together, or the journal
	// left behind would replay against nothing.
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		src := legacyDB + suffix
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Rename(src, filepath.Join(target, "control.sqlite"+suffix)); err != nil {
			return false, fmt.Errorf("stateroot: adopt legacy state: %w", err)
		}
	}
	legacyArt := filepath.Join(l.Root, "artifacts")
	if _, err := os.Stat(legacyArt); err == nil {
		if err := os.Rename(legacyArt, filepath.Join(target, "artifacts")); err != nil {
			return false, fmt.Errorf("stateroot: adopt legacy artifacts: %w", err)
		}
	}
	return true, nil
}

// writeMarker records what the project directory is, so listings can show a
// name and a path instead of a bare hash. Written once; the first record of
// where a key came from is not overwritten by later resolutions.
func (l Layout) writeMarker() error {
	marker := filepath.Join(l.ProjectDir, "project.json")
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	body, err := json.Marshal(map[string]string{"name": l.Project.Name, "source": l.Project.Source})
	if err != nil {
		return fmt.Errorf("stateroot: encode project marker: %w", err)
	}
	if err := os.WriteFile(marker, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("stateroot: write project marker: %w", err)
	}
	return nil
}

// ReadMarker reads a project directory's marker, returning its name and source.
func ReadMarker(projectDir string) (name, source string, err error) {
	raw, err := os.ReadFile(filepath.Join(projectDir, "project.json"))
	if err != nil {
		return "", "", err
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", "", fmt.Errorf("stateroot: parse project marker: %w", err)
	}
	return m["name"], m["source"], nil
}
