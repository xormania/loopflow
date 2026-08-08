package cli_test

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xormania/loopflow/internal/cli"
	"github.com/xormania/loopflow/internal/server"
	"github.com/xormania/loopflow/internal/store"
)

// remoteRunner drives the CLI against a served state root, exactly as a
// harness in another container would: URL, token, explicit project.
type remoteRunner struct {
	t    *testing.T
	root string
	url  string
}

func newRemoteRunner(t *testing.T) *remoteRunner {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	srv, err := server.New(root, "test-token")
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	t.Cleanup(srv.Close)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	t.Setenv("LOOPFLOW_TOKEN", "test-token")
	return &remoteRunner{t: t, root: root, url: hs.URL}
}

func (r *remoteRunner) run(project string, args ...string) result {
	r.t.Helper()
	var stdout, stderr bytes.Buffer
	full := append([]string{"-remote", r.url, "-project", project}, args...)
	code := cli.Run(r.t.Context(), full, &stdout, &stderr)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func (r *remoteRunner) mustRun(project string, args ...string) result {
	r.t.Helper()
	got := r.run(project, args...)
	if got.code != cli.ExitOK {
		r.t.Fatalf("loopflow -remote %s: exit %d\n%s%s", strings.Join(args, " "), got.code, got.stdout, got.stderr)
	}
	return got
}

// The whole coordination loop over HTTP: the commands, outputs, and exit
// codes a harness sees must be the ones the local store gives.
func TestRemoteQuickstart(t *testing.T) {
	r := newRemoteRunner(t)

	r.mustRun("px", "init", "p1", "-objective", "over the wire")
	r.mustRun("px", "claim", "p1", "-owner", "harness-a", "-ttl", "15m")
	r.mustRun("px", "record", "p1", "build", "-outcome", "passed")

	status := r.mustRun("px", "status", "p1").stdout
	for _, want := range []string{"over the wire", "2 (chain verified)"} {
		if !strings.Contains(status, want) {
			t.Errorf("status lacks %q:\n%s", want, status)
		}
	}
	if out := r.mustRun("px", "verify", "p1").stdout; !strings.Contains(out, "chain verified") {
		t.Errorf("verify = %q", out)
	}
	r.mustRun("px", "release", "p1", "-owner", "harness-a")
}

// A held claim must come back as exit 3 — the typed error crosses the wire.
func TestRemoteClaimConflictIsExitHeld(t *testing.T) {
	r := newRemoteRunner(t)
	r.mustRun("px", "init", "p1")
	r.mustRun("px", "claim", "p1", "-owner", "harness-a", "-ttl", "15m")

	got := r.run("px", "claim", "p1", "-owner", "harness-b", "-ttl", "15m")
	if got.code != cli.ExitHeld {
		t.Errorf("competing claim: exit %d, want %d\n%s", got.code, cli.ExitHeld, got.stderr)
	}
	if !strings.Contains(got.stderr, "harness-a") {
		t.Errorf("refusal does not name the holder:\n%s", got.stderr)
	}
}

// A refusal must stay self-explaining after the round trip.
func TestRemoteRefusalKeepsItsShape(t *testing.T) {
	r := newRemoteRunner(t)
	got := r.run("px", "-json", "record", "nope", "red", "-outcome", "passed")
	if got.code != cli.ExitFailed {
		t.Fatalf("exit = %d, want %d", got.code, cli.ExitFailed)
	}
	for _, want := range []string{"precondition-failed", "packet exists"} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("refusal lacks %q:\n%s", want, got.stderr)
		}
	}
}

// Tampered evidence on the server blocks the remote reader with exit 2.
func TestRemoteTamperedChainBlocks(t *testing.T) {
	r := newRemoteRunner(t)
	r.mustRun("px", "init", "p1")
	r.mustRun("px", "record", "p1", "red", "-outcome", "failed")

	db, err := sql.Open(store.DriverName, filepath.Join(r.root, "projects", "px", "control.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.Exec(`UPDATE events
		SET payload = replace(payload, '"outcome":"failed"', '"outcome":"passed"')
		WHERE seq = 2`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_ = db.Close()

	if got := r.run("px", "verify", "p1"); got.code != cli.ExitIntegrity {
		t.Errorf("verify after tamper: exit %d, want %d\n%s", got.code, cli.ExitIntegrity, got.stderr)
	}
}

// Artifact bytes round-trip through the server, and a wrong declared digest
// is refused there.
func TestRemoteArtifactRoundTrip(t *testing.T) {
	r := newRemoteRunner(t)
	const content = "GATE REPORT\n"
	path := filepath.Join(t.TempDir(), "report.txt")
	if err := writeFile(path, content); err != nil {
		t.Fatalf("write: %v", err)
	}

	put := r.mustRun("px", "put", path, "-class", "worker-report").stdout
	digest, _, _ := strings.Cut(strings.TrimSpace(put), " ")
	if len(digest) != 64 {
		t.Fatalf("digest = %q from %q", digest, put)
	}
	if got := r.mustRun("px", "get", digest).stdout; got != content {
		t.Errorf("get = %q, want %q", got, content)
	}
	if got := r.run("px", "put", path, "-class", "r", "-expect", strings.Repeat("0", 64)); got.code == cli.ExitOK {
		t.Error("put accepted content that did not match the declared digest")
	}
}

// Two projects through one server never see each other's packets.
func TestRemoteProjectsAreIsolated(t *testing.T) {
	r := newRemoteRunner(t)
	r.mustRun("pa", "init", "p1", "-objective", "belongs to a")
	r.mustRun("pb", "init", "p1", "-objective", "belongs to b")

	if got := r.mustRun("pb", "status", "p1").stdout; !strings.Contains(got, "belongs to b") {
		t.Errorf("project b sees the wrong packet:\n%s", got)
	}
	list := r.mustRun("pa", "projects").stdout
	for _, want := range []string{"pa", "pb"} {
		if !strings.Contains(list, want) {
			t.Errorf("projects lacks %q:\n%s", want, list)
		}
	}
}

// Sessions coordinate over the wire: a second harness is told who is live,
// with exit 3, and the listing knows the id to resume.
func TestRemoteSessionsCoordinate(t *testing.T) {
	r := newRemoteRunner(t)
	r.mustRun("px", "init", "p1")
	r.mustRun("px", "session", "p1", "-role", "auditor", "-client", "claude", "-session-id", "s-1", "-ttl", "30m")

	got := r.run("px", "session", "p1", "-role", "auditor", "-client", "grok", "-session-id", "s-2", "-ttl", "30m")
	if got.code != cli.ExitHeld {
		t.Errorf("second session: exit %d, want %d\n%s", got.code, cli.ExitHeld, got.stderr)
	}
	if !strings.Contains(got.stderr, "s-1") {
		t.Errorf("conflict does not name the session to resume:\n%s", got.stderr)
	}
	if out := r.mustRun("px", "sessions", "p1").stdout; !strings.Contains(out, "s-1") {
		t.Errorf("sessions listing lacks the live session:\n%s", out)
	}
}

// A wrong token gets nothing.
func TestRemoteWrongTokenIsRefused(t *testing.T) {
	r := newRemoteRunner(t)
	t.Setenv("LOOPFLOW_TOKEN", "not-the-token")
	if got := r.run("px", "init", "p1"); got.code == cli.ExitOK {
		t.Error("a wrong token was accepted")
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// The central coordination promise under the load Loopmaster will generate:
// many parallel workers race one claim, exactly one wins, everyone else is
// told so with exit 3.
func TestRemoteParallelClaimsHaveOneWinner(t *testing.T) {
	r := newRemoteRunner(t)
	r.mustRun("px", "init", "p1")

	const workers = 12
	codes := make(chan int, workers)
	for i := range workers {
		go func() {
			got := r.run("px", "claim", "p1", "-owner", fmt.Sprintf("harness-%d", i), "-ttl", "5m")
			codes <- got.code
		}()
	}

	winners, held := 0, 0
	for range workers {
		switch code := <-codes; code {
		case cli.ExitOK:
			winners++
		case cli.ExitHeld:
			held++
		default:
			t.Errorf("a racing claim exited %d, want %d or %d", code, cli.ExitOK, cli.ExitHeld)
		}
	}
	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1 (held: %d)", winners, held)
	}
}

// run executes locally and records remotely: output and exit pass through,
// and the attempt — including a refusal that leaves no trace in the packet —
// survives on the server for attempts to find.
func TestRemoteRunRecordsAttemptsAndRefusals(t *testing.T) {
	r := newRemoteRunner(t)
	dir := writePacket(t, "tests-frozen", 1, "head-1")

	ok := r.mustRun("px", "run", dir, "--", "/bin/echo", "hello from the wrapped tool")
	if !strings.Contains(ok.stdout, "hello from the wrapped tool") {
		t.Errorf("run did not pass output through:\n%s", ok.stdout)
	}

	refusal := r.run("px", "run", dir, "--", "/bin/sh", "-c",
		`echo "WORKFLOW-ERROR stage is frozen" >&2; exit 2`)
	if refusal.code != 2 {
		t.Errorf("run did not pass the exit code through: %d", refusal.code)
	}

	listed := r.mustRun("px", "attempts", dir).stdout
	if !strings.Contains(listed, "stage is frozen") {
		t.Errorf("the refusal did not survive to the server:\n%s", listed)
	}
}
