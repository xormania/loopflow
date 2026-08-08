// Package cli implements the loopflow command line.
//
// loopflow talks to SQLite directly. There is no daemon: the database is opened,
// migrated if needed, used, and closed on every invocation. WAL mode plus a
// busy timeout is enough for short-lived commands, and skipping the
// client/server split removes a socket, a lock, an HTTP layer, and a wire
// format from a tool one person runs from a terminal.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/xormania/loopflow/internal/artifacts"
	"github.com/xormania/loopflow/internal/attempts"
	"github.com/xormania/loopflow/internal/canonical"
	"github.com/xormania/loopflow/internal/claims"
	"github.com/xormania/loopflow/internal/events"
	"github.com/xormania/loopflow/internal/sessions"
	"github.com/xormania/loopflow/internal/stateroot"
	"github.com/xormania/loopflow/internal/store"
)

// Version is the build identity.
const Version = "0.1.0"

// Exit codes. 2 is evidence-integrity to match the convention the native
// tooling already uses (verify-hosted-ci.py, environment-facts.md 3c), so a
// script can tell "this is broken evidence" from "this was refused".
const (
	ExitOK        = 0
	ExitFailed    = 1
	ExitIntegrity = 2
	ExitHeld      = 3
	ExitUsage     = 64
)

const usageText = `loopflow — workflow coordination for agent harnesses

Usage:
  loopflow [flags] <command> [args]

Commands:
  init <packet> [-objective TEXT] [-state FILE]
        Create a packet and record its first event.

  status [<packet>]
        List packets, or show one packet's state and chain tail.

  record <packet> <phase> -outcome passed|failed|blocked
        [-issue-key KEY] [-set k=v]... [-supersedes SEQ] [-state FILE]
        Append an event.

  log <packet>
        Print the event chain as JSONL, in the native canonical form.

  verify <packet>
        Recompute every hash and link in the chain.

  put <file> -class CLASS [-media-type TYPE] [-expect DIGEST]
        Store a file in the artifact store; prints its digest.

  get <digest> [-o FILE]
        Write stored content out, after verifying it still hashes correctly.

  claim <packet> -owner ID [-ttl 15m] [-note TEXT]
        Take the claim on a packet. Exit 3 if another owner holds it.
        Claiming one you already hold extends it — that is the heartbeat.

  release <packet> -owner ID
        Drop your claim.

  session <packet> -role ROLE [-task KIND] [-cycle N] [-client C] [-session-id ID]
        [-agent-path P] [-parent ID] [-pid N] [-status running|terminal]
        [-reason TEXT] [-ttl 30m] [-takeover]
        Record which provider session is on a role-task, keyed by
        packet + role + task + cycle. Exit 3 if a different session already
        holds it — live or stale. Stale means loopflow has not heard from it, not
        that it stopped, so replacing one needs -takeover. Re-recording the
        same session id is the heartbeat.

  run <packet-dir> -- <command…>
        Run a workflow command and record the attempt: argv, exit, marker,
        output, and the packet's event count, head, and stage before and
        after. Output and exit code pass straight through, so this can sit
        in front of an existing invocation unchanged. A refusal leaves no
        trace in the packet, so this is the only place it survives.

  attempts <packet-dir>
        The failures a packet already records, and the refusals seen since.

  sessions [<packet>] [-all]
        Which sessions are running, stale, or terminal, and the id to resume.

  check <packet-dir>
        Verify a packet directory's workflow-events.jsonl in place:
        recompute every hash and link. Reads only; nothing is stored.

  projects
        List the projects this state root knows.

  version

Flags (accepted before or after the command):
  -root DIR   state root; also $LOOPFLOW_ROOT
              (default $XDG_STATE_HOME/loopflow, else ~/.local/state/loopflow)
  -project NAME
              work in a named project instead of the derived one; also
              $LOOPFLOW_PROJECT
  -json       machine-readable output

Projects:
  State is scoped per project, so packets in different repositories never
  collide. A project is derived from the git work tree enclosing the working
  directory (for run and attempts, the packet directory): identity is the
  origin remote when there is one — every clone and mount of a repository is
  the same project — else the tree's resolved path. Outside any work tree,
  the reserved project "_default". An orchestrator pins identity explicitly
  with -project or $LOOPFLOW_PROJECT.

Concurrency:
  Many loopflow processes may share one state root. Writes are serialised by
  SQLite; appends read the chain tail and write inside the same lock, so
  concurrent recorders queue rather than fork the chain.

Exit codes:
  0 ok   1 refused or failed   2 evidence-integrity   3 claim held   64 usage
`

// Run executes one command line and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	e := &env{out: stdout, errOut: stderr}

	global := flag.NewFlagSet("loopflow", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { fmt.Fprint(stderr, usageText) }
	global.StringVar(&e.root, "root", "", "state root")
	global.StringVar(&e.project, "project", "", "project name")
	global.BoolVar(&e.jsonOut, "json", false, "machine-readable output")

	if err := global.Parse(args); err != nil {
		return ExitUsage
	}
	rest := global.Args()
	if len(rest) == 0 {
		fmt.Fprint(stderr, usageText)
		return ExitUsage
	}

	cmd, cmdArgs := rest[0], rest[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return ExitOK
	case "version":
		fmt.Fprintf(stdout, "loopflow %s\n", Version)
		return ExitOK
	}

	run, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(stderr, "loopflow: unknown command %q\n\n%s", cmd, usageText)
		return ExitUsage
	}
	return run(ctx, e, cmdArgs)
}

type commandFunc func(context.Context, *env, []string) int

var commands map[string]commandFunc

func init() {
	// Declared in init to avoid an initialisation cycle: each command closes
	// over env, which references this map through Run.
	commands = map[string]commandFunc{
		"init":     cmdInit,
		"status":   cmdStatus,
		"record":   cmdRecord,
		"log":      cmdLog,
		"verify":   cmdVerify,
		"put":      cmdPut,
		"get":      cmdGet,
		"check":    cmdCheck,
		"claim":    cmdClaim,
		"run":      cmdRun,
		"attempts": cmdAttempts,
		"session":  cmdSession,
		"sessions": cmdSessions,
		"release":  cmdRelease,
		"projects": cmdProjects,
	}
}

// cmdProjects lists the projects this state root knows. It reads only the
// markers: a listing must not create state or open a database.
func cmdProjects(ctx context.Context, e *env, args []string) int {
	fs := e.flags("projects")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) != 0 {
		return e.usage(errors.New("projects takes no arguments"))
	}

	root := e.resolveRoot()
	if root == "" {
		root, err = stateroot.DefaultRoot()
		if err != nil {
			return e.fail(err)
		}
	}
	// Best effort: marking the current project helps a human; failing to
	// derive one must not break the listing.
	current, _ := e.resolveProject()

	entries, err := os.ReadDir(filepath.Join(root, "projects"))
	if err != nil && !os.IsNotExist(err) {
		return e.fail(err)
	}
	type row struct {
		Key     string `json:"key"`
		Name    string `json:"name,omitempty"`
		Source  string `json:"source,omitempty"`
		Current bool   `json:"current,omitempty"`
	}
	rows := []row{}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		name, source, _ := stateroot.ReadMarker(filepath.Join(root, "projects", ent.Name()))
		rows = append(rows, row{
			Key:     ent.Name(),
			Name:    name,
			Source:  source,
			Current: ent.Name() == current.Key,
		})
	}

	if e.jsonOut {
		e.writeJSON(e.out, map[string]any{"ok": true, "projects": rows})
		return ExitOK
	}
	if len(rows) == 0 {
		fmt.Fprintln(e.out, "no projects yet")
		return ExitOK
	}
	for _, r := range rows {
		line := r.Key
		if r.Name != "" {
			line += "  " + r.Name
		}
		if r.Source != "" {
			line += "  " + r.Source
		}
		if r.Current {
			line += "  (current)"
		}
		fmt.Fprintln(e.out, line)
	}
	return ExitOK
}

// env carries the flags and the lazily opened stores.
type env struct {
	out     io.Writer
	errOut  io.Writer
	root    string
	project string
	jsonOut bool

	// projectFrom is the directory project derivation starts from when no
	// explicit project is given. Commands that take a packet directory set
	// it, so the project follows the packet rather than the caller's cwd.
	projectFrom string

	layout   stateroot.Layout
	db       *store.DB
	log      *events.Log
	art      *artifacts.Store
	claims   *claims.Store
	sessions *sessions.Store
	attempts *attempts.Store
}

// flags returns a FlagSet that also accepts the global flags, so that both
// `loopflow -json status` and `loopflow status -json` work.
func (e *env) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("loopflow "+name, flag.ContinueOnError)
	fs.SetOutput(e.errOut)
	fs.StringVar(&e.root, "root", e.root, "state root")
	fs.StringVar(&e.project, "project", e.project, "project name")
	fs.BoolVar(&e.jsonOut, "json", e.jsonOut, "machine-readable output")
	return fs
}

// resolveRoot applies the -root, then LOOPFLOW_ROOT, then default precedence.
// The environment variable is what lets a harness point every tool it hands
// off to at one shared store without threading a flag through every call site.
func (e *env) resolveRoot() string {
	if e.root != "" {
		return e.root
	}
	return os.Getenv("LOOPFLOW_ROOT")
}

// resolveProject picks the project this invocation works in. An explicit
// -project or LOOPFLOW_PROJECT wins — under an orchestrator that is the
// assigned identity, stable across forges and mounts. Otherwise the project
// is derived from the git work tree enclosing the packet directory (for
// commands that take one) or the working directory.
func (e *env) resolveProject() (stateroot.Project, error) {
	name := e.project
	if name == "" {
		name = os.Getenv("LOOPFLOW_PROJECT")
	}
	if name != "" {
		return stateroot.NamedProject(name)
	}
	from := e.projectFrom
	if from == "" {
		wd, err := os.Getwd()
		if err != nil {
			return stateroot.Project{}, fmt.Errorf("cli: resolve working directory: %w", err)
		}
		from = wd
	}
	return stateroot.DeriveProject(from)
}

// open resolves the state root, opens the database, and migrates it. Every
// command starts here; there is no separate setup step to forget.
func (e *env) open(ctx context.Context) error {
	root := e.resolveRoot()
	proj, err := e.resolveProject()
	if err != nil {
		return err
	}

	var layout stateroot.Layout
	if root == "" {
		layout, err = stateroot.Default(proj)
	} else {
		layout, err = stateroot.New(root, proj)
	}
	if err != nil {
		return err
	}
	adopted, err := layout.Ensure()
	if err != nil {
		return err
	}
	if adopted {
		fmt.Fprintf(e.errOut, "loopflow: adopted pre-project state into project %s\n",
			stateroot.DefaultProjectKey)
	}
	e.layout = layout

	db, err := store.Open(ctx, layout.Database)
	if err != nil {
		return err
	}
	e.db = db
	if _, err := db.Migrate(ctx); err != nil {
		_ = db.Close()
		return err
	}
	e.log = events.New(db)

	art, err := artifacts.Open(db, layout.Artifacts)
	if err != nil {
		_ = db.Close()
		return err
	}
	e.art = art
	e.claims = claims.New(db)
	e.sessions = sessions.New(db)
	e.attempts = attempts.New(db)
	return nil
}

func (e *env) close() {
	if e.db != nil {
		_ = e.db.Close()
	}
}

// fail reports an error and returns the exit code its classification implies.
func (e *env) fail(err error) int {
	code := ExitFailed
	classification := "failed"
	payload := map[string]any{"ok": false, "error": err.Error()}

	var eventIntegrity *events.IntegrityError
	var artifactIntegrity *artifacts.IntegrityError
	var refusal *events.RefusalError
	var held *claims.HeldError
	var live *sessions.LiveError
	switch {
	case errors.As(err, &live):
		code = ExitHeld
		classification = "session-" + live.Existing.Liveness
		payload["existing"] = live.Existing
	case errors.As(err, &held):
		code, classification = ExitHeld, "claim-held"
		payload["packet"] = held.Packet
		payload["owner"] = held.Owner
		payload["expires"] = held.Expires
	case errors.As(err, &eventIntegrity):
		code, classification = ExitIntegrity, eventIntegrity.Classification()
		if eventIntegrity.Seq > 0 {
			payload["seq"] = eventIntegrity.Seq
		}
	case errors.As(err, &artifactIntegrity):
		code, classification = ExitIntegrity, artifactIntegrity.Classification()
		payload["digest"] = artifactIntegrity.Digest
	case errors.Is(err, events.ErrNotAChain):
		classification = "not-a-chain"
	case errors.As(err, &refusal):
		classification = refusal.Classification()
		payload["precondition"] = refusal.Precondition
		if refusal.Needed != "" {
			payload["needed"] = refusal.Needed
		}
	}
	payload["classification"] = classification

	if e.jsonOut {
		e.writeJSON(e.errOut, payload)
	} else {
		// RefusalError.Error already names the precondition and what is
		// needed, so there is nothing to add here.
		fmt.Fprintf(e.errOut, "loopflow: %s\n", err)
	}
	return code
}

func (e *env) usage(err error) int {
	fmt.Fprintf(e.errOut, "loopflow: %s\n", err)
	return ExitUsage
}

func (e *env) writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// ------------------------------------------------------------------- init ---

func cmdInit(ctx context.Context, e *env, args []string) int {
	fs := e.flags("init")
	objective := fs.String("objective", "", "what this packet is for")
	statePath := fs.String("state", "", "JSON file holding the packet's initial state")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) != 1 {
		return e.usage(errors.New("init takes exactly one packet id"))
	}
	packet := pos[0]

	state, err := readStateFile(*statePath)
	if err != nil {
		return e.usage(err)
	}

	if err := e.open(ctx); err != nil {
		return e.fail(err)
	}
	defer e.close()

	if err := e.log.CreatePacket(ctx, packet, *objective); err != nil {
		return e.fail(err)
	}
	// The same shape as the native initial event in environment-facts.md 3c.
	ev, err := e.log.Append(ctx, packet, events.Event{
		events.FieldPhase:    "init",
		events.FieldOutcome:  "passed",
		events.FieldIssueKey: "initialized",
	}, state)
	if err != nil {
		return e.fail(err)
	}

	hash, _ := ev.Hash()
	if e.jsonOut {
		e.writeJSON(e.out, map[string]any{
			"ok": true, "packet": packet, "objective": *objective, "seq": 1, "hash": hash,
		})
	} else {
		fmt.Fprintf(e.out, "created packet %s\n", packet)
		if *objective != "" {
			fmt.Fprintf(e.out, "objective  %s\n", *objective)
		}
		fmt.Fprintf(e.out, "event      seq 1  %s\n", hash)
	}
	return ExitOK
}

// ----------------------------------------------------------------- status ---

func cmdStatus(ctx context.Context, e *env, args []string) int {
	fs := e.flags("status")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) > 1 {
		return e.usage(errors.New("status takes at most one packet id"))
	}

	if err := e.open(ctx); err != nil {
		return e.fail(err)
	}
	defer e.close()

	if len(pos) == 1 {
		return e.statusOne(ctx, pos[0])
	}
	return e.statusAll(ctx)
}

func (e *env) statusAll(ctx context.Context) int {
	rows, err := e.db.ListPackets(ctx)
	if err != nil {
		return e.fail(err)
	}

	type summary struct {
		Packet    string `json:"packet"`
		Objective string `json:"objective,omitempty"`
		Events    int64  `json:"events"`
		Phase     string `json:"phase,omitempty"`
		Outcome   string `json:"outcome,omitempty"`
		Verified  bool   `json:"verified"`
		Blocked   string `json:"blocked,omitempty"`
	}
	out := make([]summary, 0, len(rows))
	blocked := false

	for _, row := range rows {
		s := summary{Packet: row.PacketID, Objective: row.Objective}
		count, err := e.db.CountEvents(ctx, row.PacketID)
		if err != nil {
			return e.fail(err)
		}
		s.Events = count

		if err := e.log.VerifyChain(ctx, row.PacketID); err != nil {
			s.Blocked = err.Error()
			blocked = true
		} else {
			s.Verified = true
			if tail, err := e.db.GetChainTail(ctx, row.PacketID); err == nil {
				if ev, err := canonical.DecodeObject([]byte(tail.Payload)); err == nil {
					s.Phase, _ = events.Event(ev).Str(events.FieldPhase)
					s.Outcome, _ = events.Event(ev).Str(events.FieldOutcome)
				}
			}
		}
		out = append(out, s)
	}

	if e.jsonOut {
		e.writeJSON(e.out, map[string]any{"ok": !blocked, "packets": out})
	} else if len(out) == 0 {
		fmt.Fprintf(e.out, "no packets yet — create one with: loopflow init <packet>\n")
	} else {
		for _, s := range out {
			status := fmt.Sprintf("%s/%s", orDash(s.Phase), orDash(s.Outcome))
			if s.Blocked != "" {
				status = "BLOCKED"
			}
			fmt.Fprintf(e.out, "%-28s %-18s %3d events  %s\n", s.Packet, status, s.Events, s.Objective)
			if s.Blocked != "" {
				fmt.Fprintf(e.out, "%-28s %s\n", "", s.Blocked)
			}
		}
	}
	if blocked {
		return ExitIntegrity
	}
	return ExitOK
}

func (e *env) statusOne(ctx context.Context, packet string) int {
	row, err := e.db.GetPacket(ctx, packet)
	if err != nil {
		return e.fail(fmt.Errorf("no such packet %q", packet))
	}

	proj, err := e.log.Project(ctx, packet)
	if err != nil {
		return e.fail(err)
	}

	var last map[string]any
	if n := len(proj.Events); n > 0 {
		last = proj.Events[n-1]
	}

	if e.jsonOut {
		e.writeJSON(e.out, map[string]any{
			"ok":           true,
			"packet":       packet,
			"objective":    row.Objective,
			"events":       len(proj.Events),
			"verified":     true,
			"last":         last,
			"state":        proj.State,
			"state_sha256": proj.StateSHA256,
		})
		return ExitOK
	}

	fmt.Fprintf(e.out, "packet     %s\n", packet)
	if row.Objective != "" {
		fmt.Fprintf(e.out, "objective  %s\n", row.Objective)
	}
	fmt.Fprintf(e.out, "events     %d (chain verified)\n", len(proj.Events))
	if last != nil {
		ev := events.Event(last)
		phase, _ := ev.Str(events.FieldPhase)
		outcome, _ := ev.Str(events.FieldOutcome)
		when, _ := ev.Str(events.FieldTime)
		fmt.Fprintf(e.out, "last       seq %d  %s/%s  %s\n", proj.LastSeq, orDash(phase), orDash(outcome), when)
		fmt.Fprintf(e.out, "hash       %s\n", proj.LastHash)
	}
	if len(proj.State) > 0 {
		pretty, err := json.MarshalIndent(proj.State, "           ", "  ")
		if err == nil {
			fmt.Fprintf(e.out, "state      %s\n", pretty)
		}
	}
	return ExitOK
}

// ----------------------------------------------------------------- record ---

func cmdRecord(ctx context.Context, e *env, args []string) int {
	fs := e.flags("record")
	outcome := fs.String("outcome", "", "passed, failed, or blocked")
	issueKey := fs.String("issue-key", "", "stable issue key for this event")
	supersedes := fs.Int64("supersedes", 0, "seq of the event this one corrects")
	statePath := fs.String("state", "", "JSON file holding the packet's new state")
	var sets kvFlags
	fs.Var(&sets, "set", "additional event field, k=v (repeatable; v is parsed as JSON when it parses)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) != 2 {
		return e.usage(errors.New("record takes a packet id and a phase"))
	}
	packet, phase := pos[0], pos[1]

	switch *outcome {
	case "passed", "failed", "blocked":
	case "":
		return e.usage(errors.New("record requires -outcome passed|failed|blocked"))
	default:
		return e.usage(fmt.Errorf("outcome %q is not passed, failed, or blocked", *outcome))
	}

	state, err := readStateFile(*statePath)
	if err != nil {
		return e.usage(err)
	}

	ev := events.Event{
		events.FieldPhase:   phase,
		events.FieldOutcome: *outcome,
	}
	if *issueKey != "" {
		ev[events.FieldIssueKey] = *issueKey
	}
	if *supersedes != 0 {
		ev[events.FieldSupersedesSeq] = *supersedes
	}
	for _, kv := range sets {
		ev[kv.key] = kv.value
	}

	if err := e.open(ctx); err != nil {
		return e.fail(err)
	}
	defer e.close()

	written, err := e.log.Append(ctx, packet, ev, state)
	if err != nil {
		return e.fail(err)
	}

	seq, _ := written.Seq()
	hash, _ := written.Hash()
	if e.jsonOut {
		e.writeJSON(e.out, map[string]any{"ok": true, "packet": packet, "event": written})
	} else {
		fmt.Fprintf(e.out, "recorded seq %d  %s/%s  %s\n", seq, phase, *outcome, hash)
	}
	return ExitOK
}

// -------------------------------------------------------------- log/verify --

func cmdLog(ctx context.Context, e *env, args []string) int {
	fs := e.flags("log")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) != 1 {
		return e.usage(errors.New("log takes exactly one packet id"))
	}

	if err := e.open(ctx); err != nil {
		return e.fail(err)
	}
	defer e.close()

	rows, err := e.db.ListEvents(ctx, pos[0])
	if err != nil {
		return e.fail(err)
	}
	// The stored payload is the exact canonical bytes that were hashed, so
	// this output is byte-identical to a native workflow-events.jsonl.
	for _, row := range rows {
		fmt.Fprintln(e.out, row.Payload)
	}
	return ExitOK
}

func cmdVerify(ctx context.Context, e *env, args []string) int {
	fs := e.flags("verify")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) != 1 {
		return e.usage(errors.New("verify takes exactly one packet id"))
	}
	packet := pos[0]

	if err := e.open(ctx); err != nil {
		return e.fail(err)
	}
	defer e.close()

	if _, err := e.db.GetPacket(ctx, packet); err != nil {
		return e.fail(fmt.Errorf("no such packet %q", packet))
	}
	if err := e.log.VerifyChain(ctx, packet); err != nil {
		return e.fail(err)
	}
	count, err := e.db.CountEvents(ctx, packet)
	if err != nil {
		return e.fail(err)
	}

	if e.jsonOut {
		e.writeJSON(e.out, map[string]any{"ok": true, "packet": packet, "events": count, "verified": true})
	} else {
		fmt.Fprintf(e.out, "%s: %d events, chain verified\n", packet, count)
	}
	return ExitOK
}

// -------------------------------------------------------------- artifacts ---

func cmdPut(ctx context.Context, e *env, args []string) int {
	fs := e.flags("put")
	class := fs.String("class", "", "what kind of evidence this is (required)")
	mediaType := fs.String("media-type", "", "media type")
	expect := fs.String("expect", "", "digest the content must have")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) != 1 {
		return e.usage(errors.New("put takes exactly one file"))
	}
	if *class == "" {
		return e.usage(errors.New("put requires -class"))
	}

	f, err := os.Open(pos[0])
	if err != nil {
		return e.fail(err)
	}
	defer func() { _ = f.Close() }()

	if err := e.open(ctx); err != nil {
		return e.fail(err)
	}
	defer e.close()

	meta := artifacts.Meta{MediaType: *mediaType, Class: *class}
	var desc artifacts.Descriptor
	if *expect != "" {
		desc, err = e.art.PutExpected(ctx, f, *expect, meta)
	} else {
		desc, err = e.art.Put(ctx, f, meta)
	}
	if err != nil {
		return e.fail(err)
	}

	if e.jsonOut {
		e.writeJSON(e.out, map[string]any{
			"ok": true, "digest": desc.Digest, "size": desc.Size,
			"media_type": desc.MediaType, "class": desc.Class,
		})
	} else {
		fmt.Fprintf(e.out, "%s  %d bytes  %s\n", desc.Digest, desc.Size, desc.Class)
	}
	return ExitOK
}

func cmdGet(ctx context.Context, e *env, args []string) int {
	fs := e.flags("get")
	outPath := fs.String("o", "", "write to this file instead of stdout")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) != 1 {
		return e.usage(errors.New("get takes exactly one digest"))
	}

	if err := e.open(ctx); err != nil {
		return e.fail(err)
	}
	defer e.close()

	rc, err := e.art.Get(ctx, pos[0])
	if err != nil {
		return e.fail(err)
	}
	defer func() { _ = rc.Close() }()

	dst := e.out
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			return e.fail(err)
		}
		defer func() { _ = f.Close() }()
		dst = f
	}
	if _, err := io.Copy(dst, rc); err != nil {
		return e.fail(err)
	}
	return ExitOK
}

// ---------------------------------------------------------------- helpers ---

// parseArgs parses flags that appear before, between, or after positional
// arguments. Go's flag package stops at the first non-flag word, which would
// make the natural `loopflow init my-packet -objective ...` silently drop the flag.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

// kvFlags collects repeated -set k=v flags.
type kvFlags []kv

type kv struct {
	key   string
	value any
}

func (f *kvFlags) String() string { return "" }

func (f *kvFlags) Set(s string) error {
	key, raw, found := strings.Cut(s, "=")
	if !found || key == "" {
		return fmt.Errorf("expected k=v, got %q", s)
	}
	// A value that parses as JSON is stored as that value, so -set cycle=3
	// records an integer and -set paths='["a"]' records a list. Anything else
	// is a string, which is what an unquoted word should be.
	value, err := canonical.Decode([]byte(raw))
	if err != nil {
		value = raw
	}
	*f = append(*f, kv{key: key, value: value})
	return nil
}

// readStateFile loads a JSON object to record as the packet's state. An empty
// path means the state is unchanged, which Append represents as nil.
func readStateFile(path string) (map[string]any, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	state, err := canonical.DecodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return state, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ------------------------------------------------------------------ claims --

func cmdClaim(ctx context.Context, e *env, args []string) int {
	fs := e.flags("claim")
	owner := fs.String("owner", "", "who is taking this packet (required)")
	note := fs.String("note", "", "what you are doing with it")
	ttl := fs.Duration("ttl", claims.DefaultTTL, "how long the claim lasts")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) != 1 {
		return e.usage(errors.New("claim takes exactly one packet id"))
	}
	if *owner == "" {
		return e.usage(errors.New("claim requires -owner"))
	}

	if err := e.open(ctx); err != nil {
		return e.fail(err)
	}
	defer e.close()

	claim, err := e.claims.Acquire(ctx, pos[0], *owner, *note, *ttl)
	if err != nil {
		return e.fail(err)
	}
	e.reportClaim(claim, "claimed")
	return ExitOK
}

func cmdRelease(ctx context.Context, e *env, args []string) int {
	fs := e.flags("release")
	owner := fs.String("owner", "", "who is releasing it (required)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) != 1 {
		return e.usage(errors.New("release takes exactly one packet id"))
	}
	if *owner == "" {
		return e.usage(errors.New("release requires -owner"))
	}

	if err := e.open(ctx); err != nil {
		return e.fail(err)
	}
	defer e.close()

	if err := e.claims.Release(ctx, pos[0], *owner); err != nil {
		return e.fail(err)
	}
	if e.jsonOut {
		e.writeJSON(e.out, map[string]any{"ok": true, "packet": pos[0], "released": true})
	} else {
		fmt.Fprintf(e.out, "released %s\n", pos[0])
	}
	return ExitOK
}

func (e *env) reportClaim(c claims.Claim, verb string) {
	if e.jsonOut {
		e.writeJSON(e.out, map[string]any{"ok": true, "claim": c})
		return
	}
	// The bare packet id first, so `PACKET=$(loopflow next -owner me | head -1)`
	// works without any parsing.
	fmt.Fprintln(e.out, c.Packet)
	fmt.Fprintf(e.out, "%s by %s until %s\n", verb, c.Owner, c.Expires.Format("2006-01-02T15:04:05Z"))
}

// ------------------------------------------------------------------ check ---

// cmdCheck verifies a packet directory where it lies. It stores nothing:
// flow-workflow.py owns that packet's state and acceptance, and loopflow reading it
// must not turn into loopflow claiming it.
func cmdCheck(ctx context.Context, e *env, args []string) int {
	fs := e.flags("check")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) != 1 {
		return e.usage(errors.New("check takes exactly one packet directory"))
	}
	dir := pos[0]

	raw, err := os.ReadFile(filepath.Join(dir, "workflow-events.jsonl"))
	if err != nil {
		return e.fail(err)
	}
	var payloads [][]byte
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		payloads = append(payloads, []byte(line))
	}

	packet := filepath.Base(filepath.Clean(dir))
	chain, err := events.VerifyPayloads(packet, payloads)
	if err != nil {
		return e.fail(err)
	}

	phases := make([]string, 0, len(chain))
	for _, ev := range chain {
		phase, _ := ev.Str(events.FieldPhase)
		phases = append(phases, phase)
	}
	var lastPhase, lastOutcome, lastTime, lastHash string
	if n := len(chain); n > 0 {
		lastPhase, _ = chain[n-1].Str(events.FieldPhase)
		lastOutcome, _ = chain[n-1].Str(events.FieldOutcome)
		lastTime, _ = chain[n-1].Str(events.FieldTime)
		lastHash, _ = chain[n-1].Hash()
	}

	if e.jsonOut {
		e.writeJSON(e.out, map[string]any{
			"ok": true, "packet": packet, "events": len(chain), "verified": true,
			"phases": phases,
			"last": map[string]any{
				"phase": lastPhase, "outcome": lastOutcome, "time": lastTime, "hash": lastHash,
			},
		})
	} else {
		fmt.Fprintf(e.out, "%s: %d events, chain verified\n", packet, len(chain))
		fmt.Fprintf(e.out, "phases  %s\n", strings.Join(phases, " → "))
		fmt.Fprintf(e.out, "last    %s/%s  %s\n", orDash(lastPhase), orDash(lastOutcome), lastTime)
		fmt.Fprintf(e.out, "hash    %s\n", lastHash)
	}
	return ExitOK
}

// ---------------------------------------------------------------- sessions --

func cmdSession(ctx context.Context, e *env, args []string) int {
	fs := e.flags("session")
	role := fs.String("role", "", "custody role the session is filling (required)")
	task := fs.String("task", "", "role-task kind, e.g. gap-review, sensitivity-review, final-audit")
	cycle := fs.Int64("cycle", 0, "your correction cycle for this role-task (not the packet's event cycle)")
	client := fs.String("client", "", "codex, grok, claude, …")
	sessionID := fs.String("session-id", "", "provider session id")
	agentPath := fs.String("agent-path", "", "collaboration task path, for internal subagents")
	parent := fs.String("parent", "", "parent session id")
	pid := fs.Int64("pid", 0, "process handle, if one is being held")
	status := fs.String("status", sessions.StatusRunning, "running or terminal")
	reason := fs.String("reason", "", "terminal reason, e.g. end_turn or max turns reached")
	note := fs.String("note", "", "free text")
	ttl := fs.Duration("ttl", sessions.DefaultTTL, "how long to assume it is live without being seen")
	takeover := fs.Bool("takeover", false, "replace a live session you have proven terminal")

	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) != 1 {
		return e.usage(errors.New("session takes exactly one packet id"))
	}
	if *role == "" {
		return e.usage(errors.New("session requires -role"))
	}

	if err := e.open(ctx); err != nil {
		return e.fail(err)
	}
	defer e.close()

	got, err := e.sessions.Record(ctx, sessions.Session{
		Packet:    pos[0],
		Role:      *role,
		Task:      *task,
		Cycle:     *cycle,
		Client:    *client,
		SessionID: *sessionID,
		AgentPath: *agentPath,
		Parent:    *parent,
		PID:       *pid,
		Status:    *status,
		Reason:    *reason,
		Note:      *note,
		TTL:       *ttl,
	}, *takeover)
	if err != nil {
		return e.fail(err)
	}

	if e.jsonOut {
		e.writeJSON(e.out, map[string]any{"ok": true, "session": got})
	} else {
		fmt.Fprintf(e.out, "%s %s %s cycle %d  %s\n",
			got.Liveness, got.Packet, got.RoleTask(), got.Cycle, describeSession(got))
	}
	return ExitOK
}

func cmdSessions(ctx context.Context, e *env, args []string) int {
	fs := e.flags("sessions")
	all := fs.Bool("all", false, "include sessions already recorded terminal")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) > 1 {
		return e.usage(errors.New("sessions takes at most one packet id"))
	}
	packet := ""
	if len(pos) == 1 {
		packet = pos[0]
	}

	if err := e.open(ctx); err != nil {
		return e.fail(err)
	}
	defer e.close()

	list, err := e.sessions.List(ctx, packet, *all)
	if err != nil {
		return e.fail(err)
	}

	if e.jsonOut {
		e.writeJSON(e.out, map[string]any{"ok": true, "sessions": list})
		return ExitOK
	}
	if len(list) == 0 {
		fmt.Fprintln(e.out, "no sessions recorded")
		return ExitOK
	}
	for _, s := range list {
		fmt.Fprintf(e.out, "%-8s %-28s %-22s cycle %-3d %s\n",
			s.Liveness, s.Packet, s.RoleTask(), s.Cycle, describeSession(s))
	}
	return ExitOK
}

// describeSession renders the identity to resume, and how long ago it spoke.
func describeSession(s sessions.Session) string {
	id := s.SessionID
	if id == "" {
		id = s.AgentPath
	}
	if id == "" {
		id = "(no id)"
	}
	out := fmt.Sprintf("%s %s", s.Client, id)
	if s.Liveness != sessions.LiveTerminal {
		out += fmt.Sprintf("  seen %s ago", time.Since(s.LastSeen).Round(time.Second))
	}
	if s.Reason != "" {
		out += "  " + s.Reason
	}
	return out
}

// ------------------------------------------------------------------- run ----

// cmdRun executes a workflow command against a packet and records the attempt.
//
// The recorder is optional and must never become a precondition for the native
// command. Everything loopflow does here is best-effort and happens around the
// child: if the state root is unusable, or the packet cannot be read, or the
// attempt cannot be persisted, the command still runs and its exit code and
// output still pass through untouched. loopflow reports its own failure on stderr
// and gets out of the way.
func cmdRun(ctx context.Context, e *env, args []string) int {
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		return e.usage(errors.New("run needs -- before the command, e.g. loopflow run <packet-dir> -- python3 …"))
	}
	own, command := args[:sep], args[sep+1:]
	if len(command) == 0 {
		return e.usage(errors.New("run needs a command after --"))
	}

	fs := e.flags("run")
	pos, err := parseArgs(fs, own)
	if err != nil {
		return ExitUsage
	}
	if len(pos) != 1 {
		return e.usage(errors.New("run takes exactly one packet directory"))
	}
	e.projectFrom = pos[0]
	dir := pos[0]

	// Best effort, before anything else can go wrong.
	before, beforeErr := readBindings(dir)

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdout = io.MultiWriter(e.out, &stdout)
	cmd.Stderr = io.MultiWriter(e.errOut, &stderr)
	cmd.Stdin = os.Stdin

	started := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(started)

	exitCode := 0
	startErr := ""
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		exitCode = exitErr.ExitCode()
	default:
		// The command could not be started. Report it the way a shell would,
		// and record that it never ran.
		fmt.Fprintf(e.errOut, "loopflow: %v\n", runErr)
		exitCode = 127
		startErr = runErr.Error()
	}

	after, afterErr := readBindings(dir)
	outcome := attempts.Outcome{
		Argv:         command,
		ExitCode:     exitCode,
		Stdout:       stdout.Bytes(),
		Stderr:       stderr.Bytes(),
		DurationMS:   elapsed.Milliseconds(),
		ToolSHA256:   hashToolInArgv(command),
		Before:       before,
		After:        after,
		AfterUnknown: afterErr != nil,
		StartErr:     startErr,
	}
	if afterErr == nil && after.Events > before.Events {
		outcome.AppendedOutcome = readLastEventOutcome(dir)
	}

	if err := e.recordAttempt(ctx, outcome, beforeErr); err != nil {
		fmt.Fprintf(e.errOut, "loopflow: the attempt was not recorded: %v\n", err)
	}
	return exitCode
}

// recordAttempt persists an attempt, opening the store only now so that a
// broken state root cannot stop the command it was wrapping.
func (e *env) recordAttempt(ctx context.Context, o attempts.Outcome, beforeErr error) error {
	if beforeErr != nil {
		return fmt.Errorf("the packet could not be read: %w", beforeErr)
	}
	if err := e.open(ctx); err != nil {
		return err
	}
	defer e.close()

	attempt, err := e.attempts.Record(ctx, o)
	if err != nil {
		return err
	}
	if e.jsonOut {
		e.writeJSON(e.errOut, map[string]any{"ok": true, "attempt": attempt})
	} else {
		fmt.Fprintf(e.errOut, "loopflow: recorded %s %s (%s)\n", attempt.ID, attempt.Kind, attempt.Transition)
	}
	return nil
}

// --------------------------------------------------------------- attempts ---

func cmdAttempts(ctx context.Context, e *env, args []string) int {
	fs := e.flags("attempts")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) != 1 {
		return e.usage(errors.New("attempts takes exactly one packet directory"))
	}
	dir := pos[0]
	e.projectFrom = dir

	current, err := readBindings(dir)
	if err != nil {
		return e.fail(err)
	}

	// The chain is verified before anything from it is shown. Presenting
	// unverified lines as durable evidence would be the thing this tool exists
	// to prevent, and reading them is not proof they hold together.
	failed, chainErr := readVerifiedFailedEvents(dir, current.Packet)

	if err := e.open(ctx); err != nil {
		return e.fail(err)
	}
	defer e.close()

	list, err := e.attempts.List(ctx, current.Packet, current)
	if err != nil {
		return e.fail(err)
	}

	var refusals, others []attempts.Attempt
	for _, a := range list {
		if a.Kind == attempts.KindRefusal {
			refusals = append(refusals, a)
			continue
		}
		others = append(others, a)
	}

	if e.jsonOut {
		payload := map[string]any{
			"ok":                        chainErr == nil,
			"packet":                    current.Packet,
			"bindings":                  current,
			"chain_verified":            chainErr == nil,
			"recorded_failed_events":    failed,
			"observed_refusals":         refusals,
			"other_recorded_attempts":   others,
			"exhaustiveness_disclaimer": exhaustivenessNote,
		}
		if chainErr != nil {
			payload["chain_error"] = chainErr.Error()
			payload["recorded_failed_events"] = nil
		}
		e.writeJSON(e.out, payload)
		if chainErr != nil {
			return e.exitFor(chainErr)
		}
		return ExitOK
	}

	fmt.Fprintf(e.out, "packet   %s\n", current.Packet)
	fmt.Fprintf(e.out, "stage    %s  (%d events, head %s)\n",
		current.Stage, current.Events, short(current.Head))

	fmt.Fprintf(e.out, "\nrecorded failed events — durable, and the chain verifies\n")
	switch {
	case chainErr != nil:
		fmt.Fprintf(e.out, "  not shown: the chain does not verify, so nothing in it is\n")
		fmt.Fprintf(e.out, "  presented as evidence — %v\n", chainErr)
	case len(failed) == 0:
		fmt.Fprintf(e.out, "  none\n")
	}
	for _, f := range failed {
		fmt.Fprintf(e.out, "  seq %-3d %-18s %-32s %s\n", f.Seq, f.Phase, f.IssueKey, f.Report)
	}

	fmt.Fprintf(e.out, "\nobserved refusals — first failed precondition only, never the full set\n")
	if len(refusals) == 0 {
		fmt.Fprintf(e.out, "  none recorded; run Flow through `loopflow run` to capture them\n")
	}
	for _, a := range refusals {
		e.printAttempt(a)
	}

	if len(others) > 0 {
		fmt.Fprintf(e.out, "\nother recorded attempts — not refusals\n")
		for _, a := range others {
			e.printAttempt(a)
		}
	}

	fmt.Fprintf(e.out, "\n%s\n", exhaustivenessNote)
	if chainErr != nil {
		return e.exitFor(chainErr)
	}
	return ExitOK
}

func (e *env) printAttempt(a attempts.Attempt) {
	state := "packet has moved since"
	if a.BindingsUnchanged {
		state = "packet unchanged since"
	}
	fmt.Fprintf(e.out, "  %s %-24s %-22s %s\n", a.At, a.Kind, a.Transition, state)
	if a.Reason != "" {
		fmt.Fprintf(e.out, "      %s %s\n", a.Marker, a.Reason)
	}
}

// exitFor maps an error to its exit code without printing it again.
func (e *env) exitFor(err error) int {
	if errors.Is(err, events.ErrEvidenceIntegrity) {
		return ExitIntegrity
	}
	return ExitFailed
}

const exhaustivenessNote = "The wrapped tool validates fail-fast: a refusal names the first unmet " +
	"precondition, not every one. This is not a complete account of what blocks the packet."

// failedEvent is a durable failure recorded in the packet's chain.
type failedEvent struct {
	Seq      int64  `json:"seq"`
	Phase    string `json:"phase"`
	IssueKey string `json:"issue_key,omitempty"`
	Report   string `json:"report,omitempty"`
	Cycle    int64  `json:"cycle,omitempty"`
}

// readVerifiedFailedEvents indexes the failures a packet records, but only
// after its chain verifies. Selecting lines with outcome "failed" out of a file
// nobody checked would let unhashed or broken data be presented as durable.
func readVerifiedFailedEvents(dir, packet string) ([]failedEvent, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "workflow-events.jsonl"))
	if err != nil {
		return nil, err
	}
	var payloads [][]byte
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		payloads = append(payloads, []byte(line))
	}

	chain, err := events.VerifyPayloads(packet, payloads)
	if err != nil {
		return nil, err
	}

	var out []failedEvent
	for _, ev := range chain {
		if outcome, _ := ev.Str(events.FieldOutcome); outcome != "failed" {
			continue
		}
		seq, _ := ev.Seq()
		phase, _ := ev.Str(events.FieldPhase)
		issue, _ := ev.Str(events.FieldIssueKey)
		report, _ := ev.Str("report")
		f := failedEvent{Seq: seq, Phase: phase, IssueKey: issue, Report: report}
		if c, ok := jsonInt(ev["cycle"]); ok {
			f.Cycle = c
		}
		out = append(out, f)
	}
	return out, nil
}

// readLastEventOutcome reads the outcome of the packet's newest event, which is
// what says whether an accepted transition recorded a pass or a failure.
func readLastEventOutcome(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "workflow-events.jsonl"))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		obj, err := canonical.DecodeObject([]byte(lines[i]))
		if err != nil {
			return ""
		}
		outcome, _ := events.Event(obj).Str(events.FieldOutcome)
		return outcome
	}
	return ""
}

// readBindings reads the packet facts an attempt is measured against.
func readBindings(dir string) (attempts.Bindings, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "workflow-state.json"))
	if err != nil {
		return attempts.Bindings{}, fmt.Errorf("read packet state: %w", err)
	}
	obj, err := canonical.DecodeObject(raw)
	if err != nil {
		return attempts.Bindings{}, fmt.Errorf("parse packet state: %w", err)
	}
	b := attempts.Bindings{}
	b.Packet, _ = obj["change_id"].(string)
	b.Stage, _ = obj["stage"].(string)
	b.Head, _ = obj["last_event_hash"].(string)
	if n, ok := jsonInt(obj["event_count"]); ok {
		b.Events = n
	}
	if b.Packet == "" {
		b.Packet = filepath.Base(filepath.Clean(dir))
	}
	return b, nil
}

func jsonInt(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

// hashToolInArgv digests the script being invoked, so an attempt records which
// version of the tool judged it.
func hashToolInArgv(argv []string) string {
	for _, arg := range argv {
		if !strings.HasSuffix(arg, ".py") {
			continue
		}
		raw, err := os.ReadFile(arg)
		if err != nil {
			return ""
		}
		return canonical.SHA256Bytes(raw)
	}
	return ""
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}
