// Package cli implements the wfc command line.
//
// wfc talks to SQLite directly. There is no daemon: the database is opened,
// migrated if needed, used, and closed on every invocation. WAL mode plus a
// busy timeout is enough for short-lived commands, and skipping the
// client/server split removes a socket, a lock, an HTTP layer, and a wire
// format from a tool one person runs from a terminal.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/xormania/wfc/internal/artifacts"
	"github.com/xormania/wfc/internal/canonical"
	"github.com/xormania/wfc/internal/claims"
	"github.com/xormania/wfc/internal/events"
	"github.com/xormania/wfc/internal/stateroot"
	"github.com/xormania/wfc/internal/store"
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

const usageText = `wfc — workflow control plane

Usage:
  wfc [flags] <command> [args]

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

  check <packet-dir>
        Verify a native Flow packet's workflow-events.jsonl in place:
        recompute every hash and link. Reads only; nothing is stored.

  version

Flags (accepted before or after the command):
  -root DIR   state root; also $WFC_ROOT
              (default $XDG_STATE_HOME/wfc, else ~/.local/state/wfc)
  -json       machine-readable output

Concurrency:
  Many wfc processes may share one state root. Writes are serialised by
  SQLite; appends read the chain tail and write inside the same lock, so
  concurrent recorders queue rather than fork the chain.

Exit codes:
  0 ok   1 refused or failed   2 evidence-integrity   3 claim held   64 usage
`

// Run executes one command line and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	e := &env{out: stdout, errOut: stderr}

	global := flag.NewFlagSet("wfc", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { fmt.Fprint(stderr, usageText) }
	global.StringVar(&e.root, "root", "", "state root")
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
		fmt.Fprintf(stdout, "wfc %s\n", Version)
		return ExitOK
	}

	run, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(stderr, "wfc: unknown command %q\n\n%s", cmd, usageText)
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
		"init":    cmdInit,
		"status":  cmdStatus,
		"record":  cmdRecord,
		"log":     cmdLog,
		"verify":  cmdVerify,
		"put":     cmdPut,
		"get":     cmdGet,
		"check":   cmdCheck,
		"claim":   cmdClaim,
		"release": cmdRelease,
	}
}

// env carries the flags and the lazily opened stores.
type env struct {
	out     io.Writer
	errOut  io.Writer
	root    string
	jsonOut bool

	layout stateroot.Layout
	db     *store.DB
	log    *events.Log
	art    *artifacts.Store
	claims *claims.Store
}

// flags returns a FlagSet that also accepts the global flags, so that both
// `wfc -json status` and `wfc status -json` work.
func (e *env) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("wfc "+name, flag.ContinueOnError)
	fs.SetOutput(e.errOut)
	fs.StringVar(&e.root, "root", e.root, "state root")
	fs.BoolVar(&e.jsonOut, "json", e.jsonOut, "machine-readable output")
	return fs
}

// open resolves the state root, opens the database, and migrates it. Every
// command starts here; there is no separate setup step to forget.
func (e *env) open(ctx context.Context) error {
	// -root wins, then WFC_ROOT, then the default. The environment variable is
	// what lets a harness point every tool it hands off to at one shared
	// store without threading a flag through every call site.
	root := e.root
	if root == "" {
		root = os.Getenv("WFC_ROOT")
	}

	var (
		layout stateroot.Layout
		err    error
	)
	if root == "" {
		layout, err = stateroot.Default()
	} else {
		layout, err = stateroot.New(root)
	}
	if err != nil {
		return err
	}
	if err := layout.Ensure(); err != nil {
		return err
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
	switch {
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
		fmt.Fprintf(e.errOut, "wfc: %s\n", err)
	}
	return code
}

func (e *env) usage(err error) int {
	fmt.Fprintf(e.errOut, "wfc: %s\n", err)
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
		fmt.Fprintf(e.out, "no packets yet — create one with: wfc init <packet>\n")
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
// make the natural `wfc init my-packet -objective ...` silently drop the flag.
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
	// The bare packet id first, so `PACKET=$(wfc next -owner me | head -1)`
	// works without any parsing.
	fmt.Fprintln(e.out, c.Packet)
	fmt.Fprintf(e.out, "%s by %s until %s\n", verb, c.Owner, c.Expires.Format("2006-01-02T15:04:05Z"))
}

// ------------------------------------------------------------------ check ---

// cmdCheck verifies a native Flow packet where it lies. It stores nothing:
// flow-workflow.py owns that packet's state and acceptance, and wfc reading it
// must not turn into wfc claiming it.
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
