// Package attempts records what was attempted against a native Flow packet and
// what happened, so a refusal survives the transcript it was printed to.
//
// A refused transition raises before it appends anything: no event, no state
// change, nothing in the packet. The reason exists only in the coordinator's
// scrollback, and once that is gone the only way to learn it again is to rerun
// the command. Recording the attempt is the whole point.
//
// # What this package is careful not to claim
//
// Native validation is fail-fast, so a refusal names the first failed
// precondition and never all of them. Nothing here is a complete account of
// what blocks a packet, and the records are labelled so a reader cannot mistake
// one for that. The structured per-precondition detail that would make such an
// account possible does not exist upstream, and inferring it by parsing prose
// would be inventing evidence.
//
// A non-zero exit and a recorded failure are also different things, and the
// difference matters more than it sounds: a refusal changed nothing, while an
// accepted failed review advanced the packet and is already durable in its
// event chain. Kind separates them.
package attempts

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/xormania/loopflow/internal/canonical"
	"github.com/xormania/loopflow/internal/store"
)

// Kinds an attempt can have. Anything that conflates these makes the record
// worse than not having it.
// Kinds. None of these says "accepted": acceptance is Flow's word and Flow's
// decision, and a recorder borrowing it would be claiming something it does not
// own. These describe only what was observed — whether the command succeeded,
// and whether the packet's chain grew.
const (
	// KindRefusal: refused, and the packet is unchanged.
	KindRefusal = "attempt-refusal"
	// KindFailureRecorded: the command succeeded in appending an event, and
	// that event records a failure. Durable in the chain already; this row only
	// says which command produced it.
	KindFailureRecorded = "failure-recorded"
	// KindTransitionRecorded: an event was appended recording a pass.
	KindTransitionRecorded = "transition-recorded"
	// KindNoTransition: the command succeeded and the chain did not grow — a
	// read-only command, or one that decided nothing needed recording.
	KindNoTransition = "no-transition"
	// KindInfrastructure: the run could not be judged on its merits.
	KindInfrastructure = "infrastructure-result"
	// KindUsage: the command was malformed and never reached Flow's own
	// validation, so it says nothing about the packet.
	KindUsage = "usage-error"
	// KindUnclassified: it failed, printed no marker, and appended nothing.
	// That is what a crash looks like as much as a refusal, and the two must
	// not be conflated — so neither is claimed.
	KindUnclassified = "unclassified-failure"
	// KindNotRun: the command could not be started at all.
	KindNotRun = "not-run"
)

// Exhaustiveness of a recorded reason.
const (
	// FirstFailure is what native refusals give: validation stops at the first
	// unmet precondition, so later ones were never evaluated.
	FirstFailure = "first_failure"
)

// Markers Flow prints. The marker-to-exit mapping is the one piece of a
// refusal that is genuinely machine-readable.
const (
	MarkerError   = "WORKFLOW-ERROR"   // exit 2
	MarkerFailed  = "WORKFLOW-FAILED"  // exit 1
	MarkerInfra   = "WORKFLOW-INFRA"   // exit 125
	MarkerBlocked = "WORKFLOW-BLOCKED" // exit 0, packet blocked
)

// tailLimit bounds the stored output. The full bytes are addressed by digest.
const tailLimit = 4096

// Bindings are the packet facts an attempt is measured against. A recorded
// reason is only the current explanation while these still hold.
type Bindings struct {
	Packet string `json:"packet"`
	Stage  string `json:"stage"`
	Events int64  `json:"events"`
	Head   string `json:"head"`
}

// Attempt is one invocation against a packet.
type Attempt struct {
	ID             string   `json:"attempt_id"`
	Packet         string   `json:"packet"`
	Transition     string   `json:"transition"`
	Argv           []string `json:"argv"`
	At             string   `json:"attempted_at"`
	DurationMS     int64    `json:"duration_ms"`
	ExitCode       int64    `json:"exit_code"`
	Marker         string   `json:"marker,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	Kind           string   `json:"record_kind"`
	Exhaustiveness string   `json:"exhaustiveness,omitempty"`
	EventAppended  bool     `json:"event_appended"`
	Before         Bindings `json:"before"`
	After          Bindings `json:"after"`
	StdoutTail     string   `json:"stdout_tail,omitempty"`
	StderrTail     string   `json:"stderr_tail,omitempty"`
	StdoutSHA256   string   `json:"stdout_sha256,omitempty"`
	StderrSHA256   string   `json:"stderr_sha256,omitempty"`
	ToolSHA256     string   `json:"tool_sha256,omitempty"`

	// BindingsUnchanged is derived at read time: the packet still has the event
	// count, head hash, and stage this attempt left it with.
	//
	// It is deliberately not called "current". A refusal can be fixed without
	// moving any of those three — removing an unexpected changed path appends
	// no event — so unchanged bindings mean the packet has not moved, never
	// that the reason still holds.
	BindingsUnchanged bool `json:"bindings_unchanged"`
}

// Outcome is what a caller observed running the command.
type Outcome struct {
	Argv       []string
	ExitCode   int
	Stdout     []byte
	Stderr     []byte
	DurationMS int64
	ToolSHA256 string
	Before     Bindings
	After      Bindings

	// AppendedOutcome is the outcome field of the event the command appended,
	// when it appended one. This is what classification should turn on: an
	// ordinary failed gap review appends outcome "failed", prints
	// GAP-REVIEW-RECORDED, and exits 0, so neither the exit code nor the marker
	// reveals that a failure was recorded.
	AppendedOutcome string

	// AfterUnknown records that the packet could not be re-read afterwards.
	// The before-state is not substituted in that case: reporting bindings that
	// were never observed would be inventing evidence.
	AfterUnknown bool

	// StartErr is set when the command could not be started at all.
	StartErr string
}

// Classify decides what an outcome was, from the exit code, the marker, and
// whether the packet actually moved.
//
// Whether an event was appended is the load-bearing signal. Flow reaches its
// central exception handler both from refusals that changed nothing and from
// paths that appended an event and wrote state first, so the exit code alone
// cannot tell those apart.
func Classify(o Outcome) (kind, marker, reason, exhaustiveness string) {
	if o.StartErr != "" {
		return KindNotRun, "", o.StartErr, ""
	}
	marker, reason = findMarker(string(o.Stderr))
	appended := !o.AfterUnknown && o.After.Events > o.Before.Events

	// What the appended event says outranks both the exit code and the marker.
	// An ordinary failed gap review exits 0 and prints GAP-REVIEW-RECORDED, so
	// classifying on those alone would call a recorded failure a success.
	if appended {
		switch o.AppendedOutcome {
		case "failed", "blocked":
			return KindFailureRecorded, marker, reason, ""
		case "passed":
			return KindTransitionRecorded, marker, reason, ""
		}
	}

	switch {
	case marker == MarkerInfra || o.ExitCode == 125:
		return KindInfrastructure, marker, reason, ""
	case appended:
		// It grew the chain but the outcome could not be read.
		return KindTransitionRecorded, marker, reason, ""
	case o.ExitCode == 0:
		return KindNoTransition, marker, reason, ""
	case marker == MarkerError || marker == MarkerFailed:
		return KindRefusal, marker, reason, FirstFailure
	case looksLikeUsage(o.Stderr):
		return KindUsage, "", firstLine(string(o.Stderr)), ""
	default:
		// Nonzero, no marker, nothing appended. A crash looks exactly like
		// this, so no claim is made about which it was.
		return KindUnclassified, "", firstLine(string(o.Stderr)), ""
	}
}

// looksLikeUsage recognises an argument-parser failure, which happens before
// Flow's own validation and so says nothing about the packet.
func looksLikeUsage(stderr []byte) bool {
	for _, line := range strings.Split(string(stderr), "\n") {
		if strings.HasPrefix(line, "usage:") || strings.HasPrefix(line, "Usage:") {
			return true
		}
	}
	return false
}

// findMarker returns the Flow marker and the message following it.
//
// Only the start of a line in stderr counts. Searching the whole of stdout and
// stderr as a substring let quoted or example marker text steer the
// classification, which is not a property a record of evidence should have.
func findMarker(stderr string) (marker, reason string) {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimRight(line, "\r")
		for _, m := range []string{MarkerError, MarkerFailed, MarkerInfra, MarkerBlocked} {
			if strings.HasPrefix(line, m) {
				return m, strings.TrimSpace(line[len(m):])
			}
		}
	}
	return "", ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// Store writes and reads attempt records.
type Store struct {
	db  *store.DB
	now func() time.Time
}

// New returns a Store using the wall clock.
func New(db *store.DB) *Store { return &Store{db: db, now: time.Now} }

// NewWithClock returns a Store reading time from now.
func NewWithClock(db *store.DB, now func() time.Time) *Store {
	return &Store{db: db, now: now}
}

const timeFormat = "2006-01-02T15:04:05.000000Z"

// Record stores one attempt. Records are never updated; a retry is a new row.
func (s *Store) Record(ctx context.Context, o Outcome) (Attempt, error) {
	if o.Before.Packet == "" {
		return Attempt{}, fmt.Errorf("attempts: the packet is not identified")
	}
	kind, marker, reason, exhaustiveness := Classify(o)
	at := s.now().UTC().Format(timeFormat)

	transition := ""
	for _, arg := range o.Argv {
		if strings.HasPrefix(arg, "-") || strings.HasSuffix(arg, ".py") ||
			strings.HasPrefix(arg, "python") {
			continue
		}
		transition = arg
		break
	}

	a := Attempt{
		Packet:         o.Before.Packet,
		Transition:     transition,
		Argv:           o.Argv,
		At:             at,
		DurationMS:     o.DurationMS,
		ExitCode:       int64(o.ExitCode),
		Marker:         marker,
		Reason:         reason,
		Kind:           kind,
		Exhaustiveness: exhaustiveness,
		EventAppended:  o.After.Events > o.Before.Events,
		Before:         o.Before,
		After:          o.After,
		StdoutTail:     tail(o.Stdout),
		StderrTail:     tail(o.Stderr),
		StdoutSHA256:   canonical.SHA256Bytes(o.Stdout),
		StderrSHA256:   canonical.SHA256Bytes(o.Stderr),
		ToolSHA256:     o.ToolSHA256,
	}

	id, err := attemptID(a)
	if err != nil {
		return Attempt{}, err
	}
	a.ID = id

	argv, err := canonical.Marshal(o.Argv)
	if err != nil {
		return Attempt{}, fmt.Errorf("attempts: encode argv: %w", err)
	}

	if err := s.db.InsertAttempt(ctx, store.InsertAttemptParams{
		AttemptID:      a.ID,
		PacketID:       a.Packet,
		Transition:     a.Transition,
		Argv:           string(argv),
		AttemptedAt:    a.At,
		DurationMs:     a.DurationMS,
		ExitCode:       a.ExitCode,
		Marker:         a.Marker,
		Reason:         a.Reason,
		RecordKind:     a.Kind,
		Exhaustiveness: a.Exhaustiveness,
		EventAppended:  boolToInt(a.EventAppended),
		EventsBefore:   a.Before.Events,
		EventsAfter:    a.After.Events,
		HeadBefore:     a.Before.Head,
		HeadAfter:      a.After.Head,
		StageBefore:    a.Before.Stage,
		StageAfter:     a.After.Stage,
		StdoutTail:     a.StdoutTail,
		StderrTail:     a.StderrTail,
		StdoutSha256:   a.StdoutSHA256,
		StderrSha256:   a.StderrSHA256,
		ToolSha256:     a.ToolSHA256,
	}); err != nil {
		return Attempt{}, fmt.Errorf("attempts: record: %w", err)
	}
	return a, nil
}

// List returns a packet's attempts oldest first, each marked according to
// whether the packet still has the bindings that attempt left behind.
//
// now is the packet's current bindings; pass the zero value when they are not
// known, and nothing will be reported current.
func (s *Store) List(ctx context.Context, packet string, current Bindings) ([]Attempt, error) {
	rows, err := s.db.ListAttempts(ctx, packet)
	if err != nil {
		return nil, err
	}
	out := make([]Attempt, 0, len(rows))
	for _, row := range rows {
		a := toAttempt(row)
		a.BindingsUnchanged = current.Head != "" &&
			a.After.Head == current.Head &&
			a.After.Events == current.Events &&
			a.After.Stage == current.Stage
		out = append(out, a)
	}
	return out, nil
}

// attemptID is derived from the attempt's own content, so the same recorded
// moment always has the same id and no randomness enters the store.
func attemptID(a Attempt) (string, error) {
	sum, err := canonical.SHA256Hex(map[string]any{
		"packet": a.Packet,
		"at":     a.At,
		"argv":   a.Argv,
		"head":   a.Before.Head,
		"events": a.Before.Events,
	})
	if err != nil {
		return "", fmt.Errorf("attempts: derive id: %w", err)
	}
	return sum[:16], nil
}

func toAttempt(row store.Attempt) Attempt {
	var argv []string
	if decoded, err := canonical.Decode([]byte(row.Argv)); err == nil {
		if list, ok := decoded.([]any); ok {
			for _, v := range list {
				if s, ok := v.(string); ok {
					argv = append(argv, s)
				}
			}
		}
	}
	return Attempt{
		ID:             row.AttemptID,
		Packet:         row.PacketID,
		Transition:     row.Transition,
		Argv:           argv,
		At:             row.AttemptedAt,
		DurationMS:     row.DurationMs,
		ExitCode:       row.ExitCode,
		Marker:         row.Marker,
		Reason:         row.Reason,
		Kind:           row.RecordKind,
		Exhaustiveness: row.Exhaustiveness,
		EventAppended:  row.EventAppended != 0,
		Before: Bindings{
			Packet: row.PacketID, Stage: row.StageBefore,
			Events: row.EventsBefore, Head: row.HeadBefore,
		},
		After: Bindings{
			Packet: row.PacketID, Stage: row.StageAfter,
			Events: row.EventsAfter, Head: row.HeadAfter,
		},
		StdoutTail:   row.StdoutTail,
		StderrTail:   row.StderrTail,
		StdoutSHA256: row.StdoutSha256,
		StderrSHA256: row.StderrSha256,
		ToolSHA256:   row.ToolSha256,
	}
}

func tail(b []byte) string {
	if len(b) <= tailLimit {
		return string(b)
	}
	return "…" + string(b[len(b)-tailLimit:])
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
