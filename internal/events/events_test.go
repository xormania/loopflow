package events_test

import (
	"errors"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xormania/wfc/internal/canonical"
	"github.com/xormania/wfc/internal/events"
	"github.com/xormania/wfc/internal/store"
)

const testPacket = "packet-1"

// fixedClock advances by one second per call so appended times are distinct
// and non-decreasing without depending on the wall clock.
func fixedClock() func() time.Time {
	base := time.Date(2026, 8, 5, 20, 32, 30, 615324000, time.UTC)
	n := 0
	return func() time.Time {
		t := base.Add(time.Duration(n) * time.Second)
		n++
		return t
	}
}

func newTestLog(t *testing.T) (*events.Log, *store.DB) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state", "control.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	log := events.NewWithClock(db, fixedClock())
	if err := log.CreatePacket(t.Context(), testPacket, "test packet"); err != nil {
		t.Fatalf("create packet: %v", err)
	}
	return log, db
}

// appendChain appends n events named phase-1..phase-n and returns them.
func appendChain(t *testing.T, log *events.Log, n int) []events.Event {
	t.Helper()
	out := make([]events.Event, 0, n)
	for i := 1; i <= n; i++ {
		ev, err := log.Append(t.Context(), testPacket, events.Event{
			events.FieldPhase:    "phase-" + strconv.Itoa(i),
			events.FieldOutcome:  "passed",
			events.FieldIssueKey: "none",
		}, map[string]any{"step": i})
		if err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
		out = append(out, ev)
	}
	return out
}

func rawPayload(t *testing.T, db *store.DB, seq int64) string {
	t.Helper()
	row, err := db.GetEvent(t.Context(), store.GetEventParams{PacketID: testPacket, Seq: seq})
	if err != nil {
		t.Fatalf("read event %d: %v", seq, err)
	}
	return row.Payload
}

func setPayload(t *testing.T, db *store.DB, seq int64, payload string) {
	t.Helper()
	if _, err := db.SQL().ExecContext(t.Context(),
		"UPDATE events SET payload = ? WHERE packet_id = ? AND seq = ?",
		payload, testPacket, seq); err != nil {
		t.Fatalf("corrupt payload %d: %v", seq, err)
	}
}

// ---------------------------------------------------------------- append ---

func TestAppendBuildsAVerifiableChain(t *testing.T) {
	log, db := newTestLog(t)
	chain := appendChain(t, log, 3)

	prev := canonical.ZeroDigest
	for i, ev := range chain {
		wantSeq := int64(i + 1)

		if seq, ok := ev.Seq(); !ok || seq != wantSeq {
			t.Errorf("event %d: seq = %v, want %d", i, ev[events.FieldSeq], wantSeq)
		}
		if got, ok := ev.Prev(); !ok || got != prev {
			t.Errorf("event %d: prev = %q, want %q", i, got, prev)
		}
		// The hash is over the event without its own hash field — the same
		// invariant the golden vector pins in internal/canonical.
		hash, ok := ev.Hash()
		if !ok {
			t.Fatalf("event %d has no hash", i)
		}
		recomputed, err := ev.ComputeHash()
		if err != nil {
			t.Fatalf("event %d: recompute: %v", i, err)
		}
		if recomputed != hash {
			t.Errorf("event %d: hash = %s, recomputed %s", i, hash, recomputed)
		}
		if !canonical.ValidDigest(hash) {
			t.Errorf("event %d: hash %q is not a lowercase-hex digest", i, hash)
		}
		prev = hash
	}

	if err := log.VerifyChain(t.Context(), testPacket); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}

	// The state row tracks the tail.
	proj, err := log.Project(t.Context(), testPacket)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if proj.LastSeq != 3 || proj.LastHash != prev {
		t.Errorf("projection tail = seq %d hash %s, want seq 3 hash %s", proj.LastSeq, proj.LastHash, prev)
	}
	if len(proj.Events) != 3 {
		t.Errorf("projection has %d events, want 3", len(proj.Events))
	}

	count, err := db.CountEvents(t.Context(), testPacket)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Errorf("stored %d events, want 3", count)
	}
}

func TestAppendDoesNotMutateTheCallersEvent(t *testing.T) {
	log, _ := newTestLog(t)
	in := events.Event{events.FieldPhase: "init"}
	if _, err := log.Append(t.Context(), testPacket, in, map[string]any{}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(in) != 1 {
		t.Errorf("caller's event was mutated: %v", in)
	}
}

func TestAppendRefusesUnknownPacket(t *testing.T) {
	log, _ := newTestLog(t)
	_, err := log.Append(t.Context(), "no-such-packet", events.Event{}, map[string]any{})
	assertRefusal(t, err, "packet exists")
}

// -------------------------------------------------------------- scenario 4 --
//
// A broken event chain blocks as evidence-integrity and is never projected as
// valid.

func TestScenario4CorruptedPayloadBlocksProjection(t *testing.T) {
	log, db := newTestLog(t)
	appendChain(t, log, 3)

	// Corrupt one byte inside the stored payload of the middle event: the
	// document still parses, and still describes an event, but it is no longer
	// the bytes that were hashed.
	original := rawPayload(t, db, 2)
	corrupted := strings.Replace(original, `"outcome":"passed"`, `"outcome":"passeE"`, 1)
	if corrupted == original {
		t.Fatalf("test setup did not corrupt anything: %s", original)
	}
	setPayload(t, db, 2, corrupted)

	err := log.VerifyChain(t.Context(), testPacket)
	assertIntegrity(t, err, 2)

	proj, err := log.Project(t.Context(), testPacket)
	assertIntegrity(t, err, 2)
	if proj != nil {
		t.Error("Project returned a projection for a broken chain")
	}
}

func TestScenario4WhitespaceOnlyRewriteIsStillCorruption(t *testing.T) {
	log, db := newTestLog(t)
	appendChain(t, log, 3)

	// This rewrite decodes to the same value and therefore rehashes to the
	// same digest. It is still a rewritten record, and the payload is required
	// to be the exact canonical bytes that were appended.
	original := rawPayload(t, db, 2)
	setPayload(t, db, 2, strings.Replace(original, `{"`, `{ "`, 1))

	assertIntegrity(t, log.VerifyChain(t.Context(), testPacket), 2)
	proj, err := log.Project(t.Context(), testPacket)
	assertIntegrity(t, err, 2)
	if proj != nil {
		t.Error("Project returned a projection for a rewritten payload")
	}
}

func TestScenario4CorruptedPrevBreaksTheLink(t *testing.T) {
	log, db := newTestLog(t)
	appendChain(t, log, 3)

	if _, err := db.SQL().ExecContext(t.Context(),
		"UPDATE events SET prev = ? WHERE packet_id = ? AND seq = 3",
		strings.Repeat("f", 64), testPacket); err != nil {
		t.Fatalf("corrupt prev: %v", err)
	}

	assertIntegrity(t, log.VerifyChain(t.Context(), testPacket), 3)
	proj, err := log.Project(t.Context(), testPacket)
	assertIntegrity(t, err, 3)
	if proj != nil {
		t.Error("Project returned a projection for a broken link")
	}
}

func TestScenario4CorruptedHashColumnIsDetected(t *testing.T) {
	log, db := newTestLog(t)
	appendChain(t, log, 3)

	if _, err := db.SQL().ExecContext(t.Context(),
		"UPDATE events SET hash = ? WHERE packet_id = ? AND seq = 1",
		strings.Repeat("a", 64), testPacket); err != nil {
		t.Fatalf("corrupt hash: %v", err)
	}
	assertIntegrity(t, log.VerifyChain(t.Context(), testPacket), 1)
}

func TestScenario4DeletedEventBreaksContiguity(t *testing.T) {
	log, db := newTestLog(t)
	appendChain(t, log, 3)

	if _, err := db.SQL().ExecContext(t.Context(),
		"DELETE FROM events WHERE packet_id = ? AND seq = 2", testPacket); err != nil {
		t.Fatalf("delete event: %v", err)
	}
	// Position 2 now holds seq 3.
	assertIntegrity(t, log.VerifyChain(t.Context(), testPacket), 3)
}

func TestScenario4TamperedStateRowBlocksProjection(t *testing.T) {
	log, db := newTestLog(t)
	appendChain(t, log, 3)

	// The chain itself is untouched; only the derived state row is rewritten.
	if _, err := db.SQL().ExecContext(t.Context(),
		"UPDATE packet_state SET state_json = ? WHERE packet_id = ?",
		`{"step":99}`, testPacket); err != nil {
		t.Fatalf("tamper state: %v", err)
	}

	if err := log.VerifyChain(t.Context(), testPacket); err != nil {
		t.Fatalf("chain should still verify: %v", err)
	}
	proj, err := log.Project(t.Context(), testPacket)
	if !errors.Is(err, events.ErrEvidenceIntegrity) {
		t.Fatalf("Project error = %v, want evidence-integrity", err)
	}
	if proj != nil {
		t.Error("Project returned a projection despite a tampered state row")
	}
}

func TestScenario4StateSequenceRewriteBlocksProjection(t *testing.T) {
	log, db := newTestLog(t)
	appendChain(t, log, 3)

	if _, err := db.SQL().ExecContext(t.Context(),
		"UPDATE packet_state SET last_seq = 2 WHERE packet_id = ?", testPacket); err != nil {
		t.Fatalf("tamper state: %v", err)
	}
	proj, err := log.Project(t.Context(), testPacket)
	if !errors.Is(err, events.ErrEvidenceIntegrity) {
		t.Fatalf("Project error = %v, want evidence-integrity", err)
	}
	if proj != nil {
		t.Error("Project returned a projection despite a stale state row")
	}
}

func TestScenario4MissingStateRowBlocksProjection(t *testing.T) {
	log, db := newTestLog(t)
	appendChain(t, log, 2)

	if _, err := db.SQL().ExecContext(t.Context(),
		"DELETE FROM packet_state WHERE packet_id = ?", testPacket); err != nil {
		t.Fatalf("delete state: %v", err)
	}

	// The chain is intact, so it still verifies; the packet is nevertheless
	// unprojectable, and that is reported rather than papered over by
	// re-deriving a state nobody recorded.
	if err := log.VerifyChain(t.Context(), testPacket); err != nil {
		t.Fatalf("chain should still verify: %v", err)
	}
	proj, err := log.Project(t.Context(), testPacket)
	if !errors.Is(err, events.ErrEvidenceIntegrity) {
		t.Fatalf("Project error = %v, want evidence-integrity", err)
	}
	if proj != nil {
		t.Error("Project returned a projection with no state row")
	}
}

// -------------------------------------------------------------- scenario 5 --
//
// A correction appends a superseding event; the earlier event is unchanged.

func TestScenario5CorrectionSupersedesWithoutRewriting(t *testing.T) {
	log, db := newTestLog(t)
	appendChain(t, log, 2)

	before := rawPayload(t, db, 2)
	beforeRow, err := db.GetEvent(t.Context(), store.GetEventParams{PacketID: testPacket, Seq: 2})
	if err != nil {
		t.Fatalf("read event 2: %v", err)
	}

	correction, err := log.Append(t.Context(), testPacket, events.Event{
		events.FieldPhase:         "phase-2",
		events.FieldOutcome:       "failed",
		events.FieldIssueKey:      "wrong-outcome-recorded",
		events.FieldSupersedesSeq: 2,
		"reason":                  "outcome was recorded from the wrong run",
	}, map[string]any{"step": 2, "corrected": true})
	if err != nil {
		t.Fatalf("append correction: %v", err)
	}

	// The superseded event's stored bytes are byte-identical.
	if after := rawPayload(t, db, 2); after != before {
		t.Errorf("superseded event was rewritten:\n before %s\n after  %s", before, after)
	}
	afterRow, err := db.GetEvent(t.Context(), store.GetEventParams{PacketID: testPacket, Seq: 2})
	if err != nil {
		t.Fatalf("re-read event 2: %v", err)
	}
	if afterRow != beforeRow {
		t.Errorf("superseded event row changed:\n before %+v\n after  %+v", beforeRow, afterRow)
	}

	// The correction names what it supersedes, and the reference is hashed
	// like every other field.
	if n, ok := correction.SupersedesSeq(); !ok || n != 2 {
		t.Errorf("correction supersedes_seq = %v, want 2", correction[events.FieldSupersedesSeq])
	}
	recomputed, err := correction.ComputeHash()
	if err != nil {
		t.Fatalf("recompute correction hash: %v", err)
	}
	if hash, _ := correction.Hash(); recomputed != hash {
		t.Errorf("correction hash %s does not cover its own fields (recomputed %s)", hash, recomputed)
	}

	// Both events project, and the chain still verifies.
	if err := log.VerifyChain(t.Context(), testPacket); err != nil {
		t.Fatalf("VerifyChain after correction: %v", err)
	}
	proj, err := log.Project(t.Context(), testPacket)
	if err != nil {
		t.Fatalf("Project after correction: %v", err)
	}
	if len(proj.Events) != 3 {
		t.Fatalf("projection has %d events, want 3", len(proj.Events))
	}
	if _, ok := proj.Events[1].SupersedesSeq(); ok {
		t.Error("the superseded event acquired a supersedes_seq field")
	}
	if n, ok := proj.Events[2].SupersedesSeq(); !ok || n != 2 {
		t.Error("the correction is not present in the projection")
	}
	if proj.LastSeq != 3 {
		t.Errorf("projection last_seq = %d, want 3", proj.LastSeq)
	}
	if corrected, ok := proj.State["corrected"]; !ok || corrected != true {
		t.Errorf("projected state did not advance to the correction: %v", proj.State)
	}
}

func TestCorrectionMustNameAnExistingEarlierEvent(t *testing.T) {
	log, _ := newTestLog(t)
	appendChain(t, log, 2)

	for _, bad := range []any{0, -1, 3, 99, "2"} {
		_, err := log.Append(t.Context(), testPacket, events.Event{
			events.FieldPhase:         "correction",
			events.FieldSupersedesSeq: bad,
		}, nil)
		assertRefusal(t, err, "supersedes_seq")
	}
}

// -------------------------------------------------------------- concurrency -
//
// Appends carrying a stale expectation are rejected and the chain is not
// forked.

func TestAppendRejectsStaleSeqAndPrev(t *testing.T) {
	log, db := newTestLog(t)
	chain := appendChain(t, log, 2)
	tailHash, _ := chain[1].Hash()

	cases := []struct {
		name  string
		event events.Event
		want  string
	}{
		{
			name:  "seq already used",
			event: events.Event{events.FieldSeq: 2, events.FieldPhase: "fork"},
			want:  "contiguous",
		},
		{
			name:  "seq skips ahead",
			event: events.Event{events.FieldSeq: 9, events.FieldPhase: "fork"},
			want:  "contiguous",
		},
		{
			name:  "prev points at an older event",
			event: events.Event{events.FieldPrev: firstHash(t, chain), events.FieldPhase: "fork"},
			want:  "links to the chain tail",
		},
		{
			name:  "prev is a zero digest on a non-empty chain",
			event: events.Event{events.FieldPrev: canonical.ZeroDigest, events.FieldPhase: "fork"},
			want:  "links to the chain tail",
		},
		{
			name:  "seq is not an integer",
			event: events.Event{events.FieldSeq: "3", events.FieldPhase: "fork"},
			want:  "seq is an integer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := log.Append(t.Context(), testPacket, tc.event, map[string]any{"forked": true})
			assertRefusal(t, err, tc.want)
		})
	}

	// Nothing was written: the chain is unforked and still ends where it did.
	count, err := db.CountEvents(t.Context(), testPacket)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("chain has %d events after refused appends, want 2", count)
	}
	if err := log.VerifyChain(t.Context(), testPacket); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	proj, err := log.Project(t.Context(), testPacket)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if proj.LastSeq != 2 || proj.LastHash != tailHash {
		t.Errorf("tail moved to seq %d hash %s, want seq 2 hash %s", proj.LastSeq, proj.LastHash, tailHash)
	}
	if _, forked := proj.State["forked"]; forked {
		t.Error("a refused append changed the projected state")
	}
}

func TestAppendRejectsMismatchedSuppliedHashAndStateDigest(t *testing.T) {
	log, _ := newTestLog(t)

	_, err := log.Append(t.Context(), testPacket, events.Event{
		events.FieldPhase: "init",
		events.FieldHash:  strings.Repeat("a", 64),
	}, map[string]any{})
	assertRefusal(t, err, "supplied hash matches")

	_, err = log.Append(t.Context(), testPacket, events.Event{
		events.FieldPhase:       "init",
		events.FieldStateSHA256: strings.Repeat("b", 64),
	}, map[string]any{})
	assertRefusal(t, err, "state_sha256 matches")
}

// A correctly supplied seq, prev, hash, and state digest are accepted: the
// checks are a compare-and-swap, not a prohibition on supplying them.
func TestAppendAcceptsCorrectlySuppliedFields(t *testing.T) {
	log, _ := newTestLog(t)
	first, err := log.Append(t.Context(), testPacket, events.Event{events.FieldPhase: "one"}, map[string]any{"n": 1})
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	firstHash, _ := first.Hash()

	state := map[string]any{"n": 2}
	stateBytes, err := canonical.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	// With seq, prev, time, and state_sha256 all pinned, the hash is fully
	// determined, so the caller can compute it too and hand it back.
	second := events.Event{
		events.FieldPhase:       "two",
		events.FieldSeq:         2,
		events.FieldPrev:        firstHash,
		events.FieldTime:        "2026-08-05T20:33:00.000000Z",
		events.FieldStateSHA256: canonical.SHA256Bytes(stateBytes),
	}
	hash, err := second.ComputeHash()
	if err != nil {
		t.Fatalf("compute hash: %v", err)
	}
	second[events.FieldHash] = hash

	got, err := log.Append(t.Context(), testPacket, second, state)
	if err != nil {
		t.Fatalf("second append: %v", err)
	}
	if seq, _ := got.Seq(); seq != 2 {
		t.Errorf("seq = %d, want 2", seq)
	}
	if gotHash, _ := got.Hash(); gotHash != hash {
		t.Errorf("hash = %s, want the caller-computed %s", gotHash, hash)
	}
	if err := log.VerifyChain(t.Context(), testPacket); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

// -------------------------------------------------------------------- time --

func TestAppendRequiresNonDecreasingTime(t *testing.T) {
	log, _ := newTestLog(t)
	if _, err := log.Append(t.Context(), testPacket, events.Event{events.FieldPhase: "one"}, nil); err != nil {
		t.Fatalf("first append: %v", err)
	}

	_, err := log.Append(t.Context(), testPacket, events.Event{
		events.FieldPhase: "two",
		events.FieldTime:  "2020-01-01T00:00:00.000000Z",
	}, nil)
	assertRefusal(t, err, "non-decreasing")
}

func TestAppendRejectsNonUTCTimeSpellings(t *testing.T) {
	log, _ := newTestLog(t)
	for _, bad := range []string{
		"2026-08-05T20:32:30.615324+00:00", // same instant, second spelling
		"2026-08-05T20:32:30",              // no zone
		"not-a-time",
		"",
	} {
		_, err := log.Append(t.Context(), testPacket, events.Event{
			events.FieldPhase: "init",
			events.FieldTime:  bad,
		}, nil)
		assertRefusal(t, err, "time is")
	}
}

// The wire form must match the native events in environment-facts.md 3c
// exactly — six fractional digits and a literal Z — because the time is hashed
// as a string.
func TestAppendedTimeUsesTheNativeWireForm(t *testing.T) {
	_, db := newTestLog(t)

	instant := time.Date(2026, 8, 5, 20, 32, 30, 615324000, time.UTC)
	log := events.NewWithClock(db, func() time.Time { return instant })

	ev, err := log.Append(t.Context(), testPacket, events.Event{events.FieldPhase: "init"}, nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got, _ := ev.Str(events.FieldTime)
	if got != "2026-08-05T20:32:30.615324Z" {
		t.Errorf("time = %q, want 2026-08-05T20:32:30.615324Z", got)
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z$`).MatchString(got) {
		t.Errorf("time %q is not in the native wire form", got)
	}
	// Sub-microsecond precision is truncated, not rounded, so the stored value
	// is always a prefix-stable rendering of the instant.
	fine := events.NewWithClock(db, func() time.Time {
		return time.Date(2026, 8, 5, 20, 32, 31, 615324999, time.UTC)
	})
	ev2, err := fine.Append(t.Context(), testPacket, events.Event{events.FieldPhase: "next"}, nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if got, _ := ev2.Str(events.FieldTime); got != "2026-08-05T20:32:31.615324Z" {
		t.Errorf("time = %q, want truncation to six digits", got)
	}
}

// ------------------------------------------------------------------- state --

func TestNilStateCarriesTheProjectionForward(t *testing.T) {
	log, _ := newTestLog(t)
	if _, err := log.Append(t.Context(), testPacket, events.Event{events.FieldPhase: "one"},
		map[string]any{"kept": "yes"}); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if _, err := log.Append(t.Context(), testPacket, events.Event{events.FieldPhase: "two"}, nil); err != nil {
		t.Fatalf("second append: %v", err)
	}

	proj, err := log.Project(t.Context(), testPacket)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if proj.State["kept"] != "yes" {
		t.Errorf("state = %v, want the earlier state carried forward", proj.State)
	}
	if proj.LastSeq != 2 {
		t.Errorf("last_seq = %d, want 2", proj.LastSeq)
	}
}

func TestAppendRefusesUnhashableState(t *testing.T) {
	log, _ := newTestLog(t)
	_, err := log.Append(t.Context(), testPacket, events.Event{events.FieldPhase: "init"},
		map[string]any{"ratio": 1.5})
	assertRefusal(t, err, "state is canonically hashable")
}

func TestAppendRefusesUnhashableEvent(t *testing.T) {
	log, _ := newTestLog(t)
	_, err := log.Append(t.Context(), testPacket, events.Event{
		events.FieldPhase: "init",
		"duration":        2.5,
	}, nil)
	assertRefusal(t, err, "canonically hashable")
}

// ------------------------------------------------------------------ misc ----

func TestProjectEmptyPacket(t *testing.T) {
	log, _ := newTestLog(t)
	proj, err := log.Project(t.Context(), testPacket)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if proj.LastSeq != 0 || proj.LastHash != canonical.ZeroDigest || len(proj.Events) != 0 {
		t.Errorf("empty packet projected as %+v", proj)
	}
}

func TestProjectUnknownPacket(t *testing.T) {
	log, _ := newTestLog(t)
	_, err := log.Project(t.Context(), "no-such-packet")
	assertRefusal(t, err, "packet exists")
}

func TestCreatePacketRefusesDuplicates(t *testing.T) {
	log, _ := newTestLog(t)
	assertRefusal(t, log.CreatePacket(t.Context(), testPacket, "test packet"), "does not already exist")
	assertRefusal(t, log.CreatePacket(t.Context(), "", ""), "not empty")
}

func TestErrorsCarryTheirClassification(t *testing.T) {
	log, _ := newTestLog(t)

	_, err := log.Append(t.Context(), "missing", events.Event{}, nil)
	var refusal *events.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("error %v is not a RefusalError", err)
	}
	if refusal.Classification() != events.ClassPrecondition {
		t.Errorf("classification = %q, want %q", refusal.Classification(), events.ClassPrecondition)
	}
	if refusal.Precondition == "" || refusal.Needed == "" {
		t.Errorf("refusal does not name its precondition and the evidence needed: %+v", refusal)
	}
	if errors.Is(err, events.ErrEvidenceIntegrity) {
		t.Error("a precondition refusal must not be classified as an integrity failure")
	}
}

// ----------------------------------------------------------------- helpers --

func firstHash(t *testing.T, chain []events.Event) string {
	t.Helper()
	h, ok := chain[0].Hash()
	if !ok {
		t.Fatal("first event has no hash")
	}
	return h
}

func assertRefusal(t *testing.T, err error, wantSubstring string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a refusal mentioning %q, got nil", wantSubstring)
	}
	var refusal *events.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("error %v is not a RefusalError", err)
	}
	if !errors.Is(err, events.ErrPrecondition) {
		t.Errorf("refusal %v does not match ErrPrecondition", err)
	}
	if !strings.Contains(err.Error(), wantSubstring) {
		t.Errorf("refusal %q does not mention %q", err, wantSubstring)
	}
}

func assertIntegrity(t *testing.T, err error, wantSeq int64) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an evidence-integrity failure at seq %d, got nil", wantSeq)
	}
	var integrity *events.IntegrityError
	if !errors.As(err, &integrity) {
		t.Fatalf("error %v is not an IntegrityError", err)
	}
	if !errors.Is(err, events.ErrEvidenceIntegrity) {
		t.Errorf("error %v does not match ErrEvidenceIntegrity", err)
	}
	if integrity.Seq != wantSeq {
		t.Errorf("integrity failure reported at seq %d, want %d (%v)", integrity.Seq, wantSeq, err)
	}
	if integrity.Classification() != events.ClassEvidenceIntegrity {
		t.Errorf("classification = %q, want %q", integrity.Classification(), events.ClassEvidenceIntegrity)
	}
}
