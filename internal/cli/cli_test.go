package cli_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xormania/wfc/internal/cli"
	"github.com/xormania/wfc/internal/store"
)

// runner drives the CLI against one state root, exactly as a shell would.
type runner struct {
	t    *testing.T
	root string
}

type result struct {
	code   int
	stdout string
	stderr string
}

func newRunner(t *testing.T) *runner {
	t.Helper()
	return &runner{t: t, root: filepath.Join(t.TempDir(), "state")}
}

func (r *runner) run(args ...string) result {
	r.t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(r.t.Context(), append([]string{"-root", r.root}, args...), &stdout, &stderr)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func (r *runner) mustRun(args ...string) result {
	r.t.Helper()
	got := r.run(args...)
	if got.code != cli.ExitOK {
		r.t.Fatalf("wfc %s: exit %d\n%s%s", strings.Join(args, " "), got.code, got.stdout, got.stderr)
	}
	return got
}

func (r *runner) json(args ...string) map[string]any {
	r.t.Helper()
	out := r.mustRun(append(args, "-json")...)
	var v map[string]any
	if err := json.Unmarshal([]byte(out.stdout), &v); err != nil {
		r.t.Fatalf("wfc %s: not JSON: %v\n%s", strings.Join(args, " "), err, out.stdout)
	}
	return v
}

// The main loop: create a packet, record progress, read it back, verify it.
func TestInitRecordStatusVerify(t *testing.T) {
	r := newRunner(t)

	r.mustRun("init", "p1", "-objective", "prove the loop")
	r.mustRun("record", "p1", "red", "-outcome", "failed", "-issue-key", "intended-red")
	r.mustRun("record", "p1", "green", "-outcome", "passed")

	got := r.json("status", "p1")
	if got["packet"] != "p1" || got["objective"] != "prove the loop" {
		t.Errorf("status = %v", got)
	}
	if n, _ := got["events"].(float64); n != 3 {
		t.Errorf("events = %v, want 3", got["events"])
	}
	if got["verified"] != true {
		t.Errorf("chain not verified: %v", got)
	}
	last, _ := got["last"].(map[string]any)
	if last["phase"] != "green" || last["outcome"] != "passed" {
		t.Errorf("last event = %v", last)
	}

	if r.json("verify", "p1")["verified"] != true {
		t.Error("verify did not confirm the chain")
	}

	// The first event has the native initial shape.
	log := r.mustRun("log", "p1").stdout
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if len(lines) != 3 {
		t.Fatalf("log has %d lines, want 3", len(lines))
	}
	if !strings.Contains(lines[0], `"issue_key":"initialized"`) ||
		!strings.Contains(lines[0], `"phase":"init"`) {
		t.Errorf("first event = %s", lines[0])
	}
	// Compact, key-sorted canonical JSON: a native workflow-events.jsonl.
	if strings.Contains(log, ": ") || strings.Contains(log, ", ") {
		t.Error("log is not compact canonical JSON")
	}

	// `wfc status` with no packet lists what exists.
	if out := r.mustRun("status").stdout; !strings.Contains(out, "p1") {
		t.Errorf("status listing = %q", out)
	}
}

// -set values that parse as JSON stay typed in the hashed event.
func TestSetValuesAreTyped(t *testing.T) {
	r := newRunner(t)
	r.mustRun("init", "p1")
	r.mustRun("record", "p1", "red", "-outcome", "failed",
		"-set", "cycle=0", "-set", "clean=true", "-set", "note=a plain word")

	last, _ := r.json("status", "p1")["last"].(map[string]any)
	if n, ok := last["cycle"].(float64); !ok || n != 0 {
		t.Errorf("cycle = %#v, want the number 0", last["cycle"])
	}
	if last["clean"] != true {
		t.Errorf("clean = %#v, want true", last["clean"])
	}
	if last["note"] != "a plain word" {
		t.Errorf("note = %#v, want a string", last["note"])
	}
}

// A correction appends; it never rewrites what came before.
func TestCorrectionAppends(t *testing.T) {
	r := newRunner(t)
	r.mustRun("init", "p1")
	r.mustRun("record", "p1", "red", "-outcome", "passed")
	before := r.mustRun("log", "p1").stdout

	r.mustRun("record", "p1", "red", "-outcome", "failed",
		"-issue-key", "wrong-outcome", "-supersedes", "2")

	after := r.mustRun("log", "p1").stdout
	if !strings.HasPrefix(after, before) {
		t.Error("a correction rewrote earlier events instead of appending")
	}
	if !strings.Contains(after, `"supersedes_seq":2`) {
		t.Error("the correction does not name what it supersedes")
	}
}

// Every refusal names the precondition that failed and what would satisfy it.
func TestRefusalNamesPrecondition(t *testing.T) {
	r := newRunner(t)

	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(),
		[]string{"-root", r.root, "-json", "record", "nope", "red", "-outcome", "passed"},
		&stdout, &stderr)
	if code != cli.ExitFailed {
		t.Fatalf("exit = %d, want %d", code, cli.ExitFailed)
	}

	var v map[string]any
	if err := json.Unmarshal(stderr.Bytes(), &v); err != nil {
		t.Fatalf("stderr is not JSON: %v\n%s", err, stderr.String())
	}
	if v["classification"] != "precondition-failed" || v["precondition"] != "packet exists" {
		t.Errorf("refusal = %v", v)
	}
	if v["needed"] == nil || v["needed"] == "" {
		t.Error("refusal does not say what is needed")
	}
}

// Tampered evidence blocks and is never reported as valid.
func TestTamperedChainBlocks(t *testing.T) {
	r := newRunner(t)
	r.mustRun("init", "p1")
	r.mustRun("record", "p1", "red", "-outcome", "failed")

	db, err := sql.Open(store.DriverName, filepath.Join(r.root, "control.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.Exec(`UPDATE events
		SET payload = replace(payload, '"outcome":"failed"', '"outcome":"passed"')
		WHERE seq = 2`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_ = db.Close()

	for _, args := range [][]string{{"status"}, {"status", "p1"}, {"verify", "p1"}} {
		got := r.run(args...)
		if got.code != cli.ExitIntegrity {
			t.Errorf("wfc %s: exit = %d, want %d", strings.Join(args, " "), got.code, cli.ExitIntegrity)
		}
		if strings.Contains(got.stdout, `"verified": true`) {
			t.Errorf("wfc %s reported tampered evidence as verified", strings.Join(args, " "))
		}
	}
}

func TestArtifactRoundTrip(t *testing.T) {
	r := newRunner(t)
	const content = "SECURITY GATE PASS\n"
	path := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	put := r.json("put", path, "-class", "worker-report")
	digest, _ := put["digest"].(string)
	if len(digest) != 64 {
		t.Fatalf("digest = %v", put["digest"])
	}
	if got := r.mustRun("get", digest).stdout; got != content {
		t.Errorf("get = %q, want %q", got, content)
	}

	// A wrong declared digest is refused.
	if got := r.run("put", path, "-class", "r", "-expect", strings.Repeat("0", 64)); got.code == cli.ExitOK {
		t.Error("put accepted content that did not match the declared digest")
	}
}

// -state records new packet state; omitting it carries the previous state.
func TestStateFileAndCarryForward(t *testing.T) {
	r := newRunner(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"stage":"tests-frozen"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	r.mustRun("init", "p1")
	r.mustRun("record", "p1", "freeze", "-outcome", "passed", "-state", statePath)
	if state, _ := r.json("status", "p1")["state"].(map[string]any); state["stage"] != "tests-frozen" {
		t.Errorf("state = %v", state)
	}

	r.mustRun("record", "p1", "green", "-outcome", "passed")
	if state, _ := r.json("status", "p1")["state"].(map[string]any); state["stage"] != "tests-frozen" {
		t.Errorf("state was not carried forward: %v", state)
	}
}

// Regression: Go's flag package stops at the first positional argument, so
// flags written after the packet id were silently dropped.
func TestFlagsAfterPositionalArguments(t *testing.T) {
	r := newRunner(t)
	r.mustRun("init", "p1", "-objective", "flag came last")
	if got := r.json("status", "p1"); got["objective"] != "flag came last" {
		t.Errorf("objective = %v; a trailing flag was dropped", got["objective"])
	}
}

func TestUsageBasics(t *testing.T) {
	r := newRunner(t)

	if got := r.run("nonesuch"); got.code != cli.ExitUsage {
		t.Errorf("unknown command: exit = %d, want %d", got.code, cli.ExitUsage)
	}
	if got := r.run("record", "p1", "red"); got.code != cli.ExitUsage {
		t.Errorf("missing -outcome: exit = %d, want %d", got.code, cli.ExitUsage)
	}
	if got := r.mustRun("version"); !strings.Contains(got.stdout, cli.Version) {
		t.Errorf("version = %q", got.stdout)
	}
}
