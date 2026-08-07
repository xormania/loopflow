package cli_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
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

// Peer harnesses must not both pick up the same packet.
func TestClaimIsExclusive(t *testing.T) {
	r := newRunner(t)
	r.mustRun("init", "p1")

	r.mustRun("claim", "p1", "-owner", "harness-1")

	taken := r.run("claim", "p1", "-owner", "harness-2")
	if taken.code != cli.ExitHeld {
		t.Fatalf("exit = %d, want %d", taken.code, cli.ExitHeld)
	}
	if !strings.Contains(taken.stderr, "harness-1") {
		t.Errorf("stderr = %q, want it to name the holder", taken.stderr)
	}

	// Re-claiming your own is the heartbeat, not a conflict.
	r.mustRun("claim", "p1", "-owner", "harness-1")

	// Only the holder may release.
	if got := r.run("release", "p1", "-owner", "harness-2"); got.code != cli.ExitHeld {
		t.Errorf("release by a non-holder: exit = %d, want %d", got.code, cli.ExitHeld)
	}
	r.mustRun("release", "p1", "-owner", "harness-1")
	r.mustRun("claim", "p1", "-owner", "harness-2")
}

// `wfc check` verifies a native packet where it lies, storing nothing.
// flow-workflow.py owns those packets; wfc reading one must not become wfc
// claiming it.
func TestCheckVerifiesANativePacketInPlace(t *testing.T) {
	r := newRunner(t)
	r.mustRun("init", "p1")
	r.mustRun("record", "p1", "red", "-outcome", "failed")
	r.mustRun("record", "p1", "test-freeze", "-outcome", "passed")

	// wfc's own output is the native format, so it round-trips through a file.
	dir := filepath.Join(t.TempDir(), "packet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	jsonl := filepath.Join(dir, "workflow-events.jsonl")
	if err := os.WriteFile(jsonl, []byte(r.mustRun("log", "p1").stdout), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := r.json("check", dir)
	if got["verified"] != true {
		t.Errorf("check = %v", got)
	}
	if n, _ := got["events"].(float64); n != 3 {
		t.Errorf("events = %v, want 3", got["events"])
	}
	phases, _ := got["phases"].([]any)
	if len(phases) != 3 || phases[0] != "init" || phases[2] != "test-freeze" {
		t.Errorf("phases = %v", got["phases"])
	}

	// A single altered byte is caught, and named by seq.
	raw, err := os.ReadFile(jsonl)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	tampered := strings.Replace(string(raw), `"outcome":"failed"`, `"outcome":"passed"`, 1)
	if err := os.WriteFile(jsonl, []byte(tampered), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	bad := r.run("check", dir)
	if bad.code != cli.ExitIntegrity {
		t.Errorf("exit = %d, want %d", bad.code, cli.ExitIntegrity)
	}
	if !strings.Contains(bad.stderr, "event 2") {
		t.Errorf("stderr = %q, want it to name the failing seq", bad.stderr)
	}
}

// The registry answers "is the worker I launched still alive, and what do I
// resume?" — the question whose absence cost a duplicate Grok audit.
func TestSessionGuardsAgainstADuplicateLaunch(t *testing.T) {
	r := newRunner(t)
	const packet = "g1-plan-authority-refreeze-1"

	r.mustRun("session", packet, "-role", "auditor", "-cycle", "3",
		"-client", "grok", "-session-id", "sess-original")

	// About to launch a replacement: refused, and told what to resume instead.
	dup := r.run("session", packet, "-role", "auditor", "-cycle", "3",
		"-client", "grok", "-session-id", "sess-replacement")
	if dup.code != cli.ExitHeld {
		t.Fatalf("exit = %d, want %d", dup.code, cli.ExitHeld)
	}
	if !strings.Contains(dup.stderr, "sess-original") {
		t.Errorf("stderr = %q, want the incumbent session id", dup.stderr)
	}

	// Re-recording the same session is the heartbeat.
	r.mustRun("session", packet, "-role", "auditor", "-cycle", "3",
		"-client", "grok", "-session-id", "sess-original")

	// A different role is unaffected.
	r.mustRun("session", packet, "-role", "test_author", "-cycle", "0",
		"-client", "codex", "-agent-path", "/root/g1_plan_test_author_refreeze")

	list := r.mustRun("sessions", packet).stdout
	if !strings.Contains(list, "sess-original") || !strings.Contains(list, "/root/g1_plan_test_author_refreeze") {
		t.Errorf("sessions = %q", list)
	}

	// Terminal sessions drop out of the default view but stay on record.
	r.mustRun("session", packet, "-role", "auditor", "-cycle", "3",
		"-client", "grok", "-session-id", "sess-original",
		"-status", "terminal", "-reason", "end_turn")
	if got := r.mustRun("sessions", packet).stdout; strings.Contains(got, "sess-original") {
		t.Errorf("terminal session still listed: %q", got)
	}
	if got := r.mustRun("sessions", packet, "-all").stdout; !strings.Contains(got, "end_turn") {
		t.Errorf("-all did not show the terminal session: %q", got)
	}

	// Once terminal, the replacement is free to take the role-task.
	r.mustRun("session", packet, "-role", "auditor", "-cycle", "3",
		"-client", "grok", "-session-id", "sess-replacement")
}

// -takeover is for when the caller has proven the old process dead by means
// wfc cannot see.
func TestSessionTakeover(t *testing.T) {
	r := newRunner(t)
	r.mustRun("session", "p1", "-role", "auditor", "-client", "grok", "-session-id", "old")

	if got := r.run("session", "p1", "-role", "auditor", "-client", "grok", "-session-id", "new"); got.code != cli.ExitHeld {
		t.Fatalf("exit = %d, want %d", got.code, cli.ExitHeld)
	}
	r.mustRun("session", "p1", "-role", "auditor", "-client", "grok", "-session-id", "new", "-takeover")

	if got := r.mustRun("sessions", "p1").stdout; !strings.Contains(got, "new") {
		t.Errorf("sessions = %q", got)
	}
}

// writePacket makes a minimal native packet directory.
func writePacket(t *testing.T, stage string, events int64, head string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "packet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	state := fmt.Sprintf(
		`{"change_id":"pkt-1","stage":%q,"event_count":%d,"last_event_hash":%q}`,
		stage, events, head)
	if err := os.WriteFile(filepath.Join(dir, "workflow-state.json"), []byte(state), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workflow-events.jsonl"), nil, 0o600); err != nil {
		t.Fatalf("write events: %v", err)
	}
	return dir
}

// A refused transition leaves no trace in the packet, so the wrapper is the
// only place its reason survives. It must also stay transparent.
func TestRunRecordsARefusalAndPassesTheExitThrough(t *testing.T) {
	r := newRunner(t)
	dir := writePacket(t, "tests-frozen", 10, "abc123")

	got := r.run("run", dir, "--", "sh", "-c",
		`echo "WORKFLOW-ERROR stage tests-frozen does not permit this transition" >&2; exit 2`)
	if got.code != 2 {
		t.Errorf("exit = %d, want the child's 2", got.code)
	}
	if !strings.Contains(got.stderr, "does not permit this transition") {
		t.Errorf("the child's output did not pass through: %q", got.stderr)
	}

	out := r.mustRun("attempts", dir).stdout
	if !strings.Contains(out, "attempt-refusal") {
		t.Errorf("attempts = %q, want an attempt-refusal", out)
	}
	if !strings.Contains(out, "does not permit this transition") {
		t.Errorf("the reason was not recorded: %q", out)
	}
	// It must never read as a complete account of what blocks the packet.
	if !strings.Contains(out, "fail-fast") {
		t.Error("no fail-fast disclaimer")
	}
}

// A refusal changed nothing; an accepted failed review advanced the packet and
// is durable in its chain. Conflating them makes the record worse than nothing.
func TestRunSeparatesRefusalFromAcceptedFailure(t *testing.T) {
	r := newRunner(t)
	dir := writePacket(t, "red-recorded", 3, "head-3")

	// An ordinary failed gap review appends outcome "failed", prints
	// GAP-REVIEW-RECORDED, and exits 0. Neither the exit code nor the marker
	// reveals that a failure was recorded — only the appended event does.
	advance := fmt.Sprintf(
		`printf '%%s' '{"change_id":"pkt-1","stage":"gap-repair-required","event_count":4,"last_event_hash":"head-4"}' > %s;`+
			`printf '%%s\n' '{"issue_key":"seam-parity","outcome":"failed","phase":"test-gap-review","seq":4}' >> %s;`+
			`echo GAP-REVIEW-RECORDED`,
		filepath.Join(dir, "workflow-state.json"), filepath.Join(dir, "workflow-events.jsonl"))

	got := r.run("run", dir, "--", "sh", "-c", advance)
	if got.code != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "failure-recorded") {
		t.Errorf("classified as %q, want failure-recorded", got.stderr)
	}
	if strings.Contains(got.stderr, "accepted") {
		t.Error("the recorder used Flow's word for a decision it does not own")
	}
}

// An optional recorder must never become a precondition for the command it
// wraps. A broken state root must not stop Flow from running.
func TestRunStillRunsWhenTheRecorderCannotWork(t *testing.T) {
	dir := writePacket(t, "tests-frozen", 1, "head-1")

	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(),
		[]string{"-root", "/dev/null/impossible", "run", dir, "--", "sh", "-c", "echo ran; exit 7"},
		&stdout, &stderr)

	if code != 7 {
		t.Errorf("exit = %d, want the child's 7", code)
	}
	if !strings.Contains(stdout.String(), "ran") {
		t.Errorf("the child did not run: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "not recorded") {
		t.Errorf("wfc did not say it failed to record: %q", stderr.String())
	}
}

// A stored reason explains the packet only while the packet still has the
// bindings it had when the reason was produced.
func TestAttemptsGoStaleWhenThePacketMoves(t *testing.T) {
	r := newRunner(t)
	dir := writePacket(t, "tests-frozen", 10, "head-10")

	r.run("run", dir, "--", "sh", "-c", `echo "WORKFLOW-ERROR nope" >&2; exit 2`)
	if out := r.mustRun("attempts", dir).stdout; !strings.Contains(out, "packet unchanged since") {
		t.Errorf("bindings not reported unchanged straight after recording: %q", out)
	}

	// The packet advances; the recorded reason is still true about its moment
	// but no longer describes now.
	moved := `{"change_id":"pkt-1","stage":"green-recorded","event_count":11,"last_event_hash":"head-11"}`
	if err := os.WriteFile(filepath.Join(dir, "workflow-state.json"), []byte(moved), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out := r.mustRun("attempts", dir).stdout; !strings.Contains(out, "packet has moved since") {
		t.Errorf("bindings still reported unchanged after the packet moved: %q", out)
	}
}

// The durable failures — the expensive category — are indexed from the chain
// the packet already keeps.
func TestAttemptsIndexesRecordedFailedEvents(t *testing.T) {
	r := newRunner(t)
	dir := writePacket(t, "tests-frozen", 2, "head-2")

	// Build a real, hash-linked chain with wfc itself, then plant it.
	r.mustRun("init", "src")
	r.mustRun("record", "src", "test-gap-review", "-outcome", "failed",
		"-issue-key", "product-negative-seam-parity", "-set", "report=results/grok/gap-cycle-0.md")
	r.mustRun("record", "src", "test-freeze", "-outcome", "passed")
	chain := r.mustRun("log", "src").stdout
	if err := os.WriteFile(filepath.Join(dir, "workflow-events.jsonl"), []byte(chain), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	out := r.mustRun("attempts", dir).stdout
	if !strings.Contains(out, "product-negative-seam-parity") ||
		!strings.Contains(out, "results/grok/gap-cycle-0.md") {
		t.Errorf("failed event not indexed: %q", out)
	}
	if strings.Contains(out, "test-freeze") {
		t.Errorf("a passed event was listed as a failure: %q", out)
	}

	// Unverified lines are never presented as durable evidence.
	unhashed := `{"issue_key":"invented","outcome":"failed","phase":"test-gap-review"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "workflow-events.jsonl"), []byte(unhashed), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	bad := r.run("attempts", dir)
	if bad.code == cli.ExitOK {
		t.Error("attempts succeeded over an unverifiable chain")
	}
	if strings.Contains(bad.stdout, "invented") {
		t.Error("an unverified line was presented as a durable failed event")
	}
}

// An unhashed activity log is not damaged evidence. Reporting it as an
// integrity failure would send a reader hunting for corruption in a file that
// never claimed to be a chain.
func TestCheckSeparatesNotAChainFromCorruption(t *testing.T) {
	r := newRunner(t)
	dir := filepath.Join(t.TempDir(), "packet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	log := `{"time":"2026-08-04T15:49:00Z","phase":"plan","actor":"codex"}` + "\n" +
		`{"time":"2026-08-04T15:50:00Z","phase":"probe","actor":"codex"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "workflow-events.jsonl"), []byte(log), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := r.run("check", dir)
	if got.code != cli.ExitFailed {
		t.Errorf("exit = %d, want %d (not %d, which means damaged evidence)",
			got.code, cli.ExitFailed, cli.ExitIntegrity)
	}
	if !strings.Contains(got.stderr, "not a hash-linked event chain") {
		t.Errorf("stderr = %q", got.stderr)
	}
	if strings.Contains(got.stderr, "evidence-integrity") {
		t.Error("a file that was never a chain was reported as damaged evidence")
	}
}
