package events

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/xormania/wfc/internal/canonical"
	"github.com/xormania/wfc/internal/store"
)

// Log is the append-only event chain over a control-plane database.
type Log struct {
	db  *store.DB
	now func() time.Time
}

// New returns a Log using the wall clock.
func New(db *store.DB) *Log { return &Log{db: db, now: time.Now} }

// NewWithClock returns a Log reading time from now. Times are still required
// to be non-decreasing per packet, so a clock that goes backwards produces
// refusals rather than a reordered chain.
func NewWithClock(db *store.DB, now func() time.Time) *Log {
	return &Log{db: db, now: now}
}

// Projection is the current state of a packet, derived from its chain and
// checked against the stored state row. It is only ever returned when the
// chain verifies and the two agree.
type Projection struct {
	PacketID    string
	State       map[string]any
	StateSHA256 string
	LastSeq     int64
	LastHash    string
	Events      []Event
}

// CreatePacket registers a packet so events can be appended to it.
func (l *Log) CreatePacket(ctx context.Context, packetID, objective string) error {
	if packetID == "" {
		return &RefusalError{
			Precondition: "packet id is not empty",
			Needed:       "a non-empty packet id",
		}
	}
	return l.db.Tx(ctx, func(q *store.Queries) error {
		switch _, err := q.GetPacket(ctx, packetID); {
		case err == nil:
			return &RefusalError{
				PacketID:     packetID,
				Precondition: "packet does not already exist",
				Needed:       "a packet id that is not in use",
			}
		case errors.Is(err, sql.ErrNoRows):
		default:
			return err
		}
		return q.CreatePacket(ctx, store.CreatePacketParams{
			PacketID:  packetID,
			CreatedAt: FormatTime(l.now()),
			Objective: objective,
		})
	})
}

// Append adds one event to a packet's chain and replaces its projected state,
// in a single transaction.
//
// The caller may leave seq, prev, time, state_sha256, and hash unset, in which
// case they are derived. Supplying them is a compare-and-swap: a value that
// disagrees with what the chain requires is refused rather than overwritten,
// which is how a writer working from a stale read is caught. The primary key
// on (packet_id, seq) is the backstop beneath that check.
//
// A nil state means the event does not change the packet's state; the stored
// state is carried forward unchanged. A packet's first event with a nil state
// starts from the empty object.
//
// The input event is never mutated. The completed event is returned.
func (l *Log) Append(ctx context.Context, packetID string, in Event, state map[string]any) (Event, error) {
	var out Event
	err := l.db.Tx(ctx, func(q *store.Queries) error {
		ev, err := l.prepare(ctx, q, packetID, in, state)
		if err != nil {
			return err
		}
		out = ev.event
		return l.write(ctx, q, packetID, ev)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// prepared is a validated event together with the derived rows it writes.
type prepared struct {
	event       Event
	seq         int64
	prev        string
	hash        string
	time        string
	payload     string
	stateJSON   string
	stateSHA256 string
}

func (l *Log) prepare(ctx context.Context, q *store.Queries, packetID string, in Event, state map[string]any) (prepared, error) {
	var zero prepared

	if _, err := q.GetPacket(ctx, packetID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return zero, &RefusalError{
				PacketID:     packetID,
				Precondition: "packet exists",
				Needed:       "create the packet before appending events",
			}
		}
		return zero, err
	}

	expectedSeq := int64(1)
	expectedPrev := canonical.ZeroDigest
	var tailTime time.Time
	hasTail := false

	switch tail, err := q.GetChainTail(ctx, packetID); {
	case err == nil:
		hasTail = true
		expectedSeq = tail.Seq + 1
		expectedPrev = tail.Hash
		tailTime, err = ParseTime(tail.Time)
		if err != nil {
			return zero, integrityf(packetID, tail.Seq, "stored event time is unreadable", "%v", err)
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return zero, err
	}

	ev := in.Clone()

	// seq and prev: compare-and-swap against the chain tail.
	if raw, present := ev[FieldSeq]; present {
		got, ok := asInt64(raw)
		if !ok {
			return zero, &RefusalError{
				PacketID:     packetID,
				Precondition: "seq is an integer",
				Actual:       fmt.Sprintf("%v", raw),
				Needed:       "an integer seq, or none at all",
			}
		}
		if got != expectedSeq {
			return zero, &RefusalError{
				PacketID:     packetID,
				Precondition: "append is contiguous with the chain tail",
				Expected:     fmt.Sprint(expectedSeq),
				Actual:       fmt.Sprint(got),
				Needed:       "re-read the chain tail and retry against the current seq",
			}
		}
	}
	ev[FieldSeq] = expectedSeq

	if raw, present := ev[FieldPrev]; present {
		got, ok := raw.(string)
		if !ok || got != expectedPrev {
			return zero, &RefusalError{
				PacketID:     packetID,
				Precondition: "append links to the chain tail",
				Expected:     expectedPrev,
				Actual:       fmt.Sprintf("%v", raw),
				Needed:       "re-read the chain tail and retry against the current hash",
			}
		}
	}
	ev[FieldPrev] = expectedPrev

	// time: supplied or taken from the clock, then normalised to the wire form
	// before the monotonicity check, so the value compared is the value hashed.
	var eventTime time.Time
	if raw, present := ev[FieldTime]; present {
		s, ok := raw.(string)
		if !ok {
			return zero, &RefusalError{
				PacketID:     packetID,
				Precondition: "time is a string",
				Actual:       fmt.Sprintf("%v", raw),
				Needed:       "an RFC3339 UTC time, or none at all",
			}
		}
		parsed, err := ParseTime(s)
		if err != nil {
			return zero, &RefusalError{
				PacketID:     packetID,
				Precondition: "time is RFC3339 UTC",
				Actual:       s,
				Needed:       err.Error(),
			}
		}
		eventTime = parsed
	} else {
		eventTime = l.now()
	}
	formatted := FormatTime(eventTime)
	eventTime, err := ParseTime(formatted)
	if err != nil {
		return zero, fmt.Errorf("events: formatting time: %w", err)
	}
	if hasTail && eventTime.Before(tailTime) {
		return zero, &RefusalError{
			PacketID:     packetID,
			Precondition: "time is non-decreasing within the packet",
			Expected:     "at or after " + FormatTime(tailTime),
			Actual:       formatted,
			Needed:       "an event time no earlier than the chain tail's",
		}
	}
	ev[FieldTime] = formatted

	// state: marshalled fresh, or carried forward when the event does not
	// change it.
	stateJSON, stateSHA, err := l.resolveState(ctx, q, packetID, state, hasTail)
	if err != nil {
		return zero, err
	}
	if raw, present := ev[FieldStateSHA256]; present {
		got, ok := raw.(string)
		if !ok || got != stateSHA {
			return zero, &RefusalError{
				PacketID:     packetID,
				Precondition: "state_sha256 matches the supplied state",
				Expected:     stateSHA,
				Actual:       fmt.Sprintf("%v", raw),
				Needed:       "omit state_sha256, or supply the hash of the state being written",
			}
		}
	}
	ev[FieldStateSHA256] = stateSHA

	// supersedes_seq: a correction must name an event that exists and precedes
	// it. Nothing is rewritten; the superseded event stays exactly as it is.
	if raw, present := ev[FieldSupersedesSeq]; present {
		n, ok := asInt64(raw)
		if !ok || n < 1 || n >= expectedSeq {
			return zero, &RefusalError{
				PacketID:     packetID,
				Precondition: "supersedes_seq names an earlier event",
				Expected:     fmt.Sprintf("1..%d", expectedSeq-1),
				Actual:       fmt.Sprintf("%v", raw),
				Needed:       "the seq of an event already in this chain",
			}
		}
		if _, err := q.GetEvent(ctx, store.GetEventParams{PacketID: packetID, Seq: n}); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return zero, &RefusalError{
					PacketID:     packetID,
					Precondition: "superseded event exists",
					Actual:       fmt.Sprint(n),
					Needed:       "the seq of an event already in this chain",
				}
			}
			return zero, err
		}
		ev[FieldSupersedesSeq] = n
	}

	// hash last: it covers everything decided above.
	hash, err := ev.ComputeHash()
	if err != nil {
		return zero, &RefusalError{
			PacketID:     packetID,
			Precondition: "event is canonically hashable",
			Actual:       err.Error(),
			Needed:       "an event whose fields are JSON objects, arrays, strings, integers, booleans, or null",
		}
	}
	if raw, present := ev[FieldHash]; present {
		got, ok := raw.(string)
		if !ok || got != hash {
			return zero, &RefusalError{
				PacketID:     packetID,
				Precondition: "supplied hash matches the event content",
				Expected:     hash,
				Actual:       fmt.Sprintf("%v", raw),
				Needed:       "omit hash, or supply the hash of the event as written",
			}
		}
	}
	ev[FieldHash] = hash

	payload, err := ev.Canonical()
	if err != nil {
		return zero, fmt.Errorf("events: canonicalising event: %w", err)
	}

	return prepared{
		event:       ev,
		seq:         expectedSeq,
		prev:        expectedPrev,
		hash:        hash,
		time:        formatted,
		payload:     string(payload),
		stateJSON:   stateJSON,
		stateSHA256: stateSHA,
	}, nil
}

func (l *Log) resolveState(ctx context.Context, q *store.Queries, packetID string, state map[string]any, hasTail bool) (stateJSON, stateSHA string, err error) {
	if state == nil {
		if !hasTail {
			state = map[string]any{}
		} else {
			row, err := q.GetPacketState(ctx, packetID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return "", "", integrityf(packetID, 0, "packet has events but no state row", "")
				}
				return "", "", err
			}
			return row.StateJson, row.StateSha256, nil
		}
	}
	b, err := canonical.Marshal(state)
	if err != nil {
		return "", "", &RefusalError{
			PacketID:     packetID,
			Precondition: "state is canonically hashable",
			Actual:       err.Error(),
			Needed:       "a state object whose values are JSON objects, arrays, strings, integers, booleans, or null",
		}
	}
	return string(b), canonical.SHA256Bytes(b), nil
}

func (l *Log) write(ctx context.Context, q *store.Queries, packetID string, p prepared) error {
	if err := q.InsertEvent(ctx, store.InsertEventParams{
		PacketID:    packetID,
		Seq:         p.seq,
		Hash:        p.hash,
		Prev:        p.prev,
		Time:        p.time,
		Payload:     p.payload,
		StateSha256: p.stateSHA256,
	}); err != nil {
		return fmt.Errorf("events: append to %s: %w", packetID, err)
	}
	if err := q.UpsertPacketState(ctx, store.UpsertPacketStateParams{
		PacketID:    packetID,
		StateJson:   p.stateJSON,
		StateSha256: p.stateSHA256,
		LastSeq:     p.seq,
		LastHash:    p.hash,
	}); err != nil {
		return fmt.Errorf("events: record state for %s: %w", packetID, err)
	}
	return nil
}

// VerifyChain recomputes every hash and link in a packet's chain.
//
// It returns an *IntegrityError naming the first seq at which the chain fails.
// A packet with no events verifies: nothing is claimed, so nothing is broken.
func (l *Log) VerifyChain(ctx context.Context, packetID string) error {
	rows, err := l.db.ListEvents(ctx, packetID)
	if err != nil {
		return err
	}

	prevHash := canonical.ZeroDigest
	var prevTime time.Time

	for i, row := range rows {
		wantSeq := int64(i + 1)
		if row.Seq != wantSeq {
			return integrityf(packetID, row.Seq, "chain is not contiguous",
				"expected seq %d at position %d", wantSeq, i+1)
		}

		obj, err := canonical.DecodeObject([]byte(row.Payload))
		if err != nil {
			return integrityf(packetID, row.Seq, "stored payload is not decodable JSON", "%v", err)
		}
		event := Event(obj)

		// The payload must be the exact canonical bytes that were hashed, not
		// merely a document that decodes to the same value: a rewritten row is
		// a rewritten record even when it happens to mean the same thing.
		reencoded, err := event.Canonical()
		if err != nil {
			return integrityf(packetID, row.Seq, "stored payload is not canonically encodable", "%v", err)
		}
		if string(reencoded) != row.Payload {
			return integrityf(packetID, row.Seq, "stored payload is not in canonical form", "")
		}

		if err := checkColumn(packetID, row.Seq, FieldSeq, fmt.Sprint(row.Seq), func() (string, bool) {
			n, ok := event.Seq()
			return fmt.Sprint(n), ok
		}); err != nil {
			return err
		}
		if err := checkColumn(packetID, row.Seq, FieldPrev, row.Prev, event.Prev); err != nil {
			return err
		}
		if err := checkColumn(packetID, row.Seq, FieldTime, row.Time, func() (string, bool) {
			return event.Str(FieldTime)
		}); err != nil {
			return err
		}
		if err := checkColumn(packetID, row.Seq, FieldStateSHA256, row.StateSha256, event.StateSHA256); err != nil {
			return err
		}
		if err := checkColumn(packetID, row.Seq, FieldHash, row.Hash, event.Hash); err != nil {
			return err
		}

		computed, err := event.ComputeHash()
		if err != nil {
			return integrityf(packetID, row.Seq, "stored event cannot be rehashed", "%v", err)
		}
		if computed != row.Hash {
			return integrityf(packetID, row.Seq, "recomputed hash does not match the stored hash",
				"recomputed %s, stored %s", computed, row.Hash)
		}

		if row.Prev != prevHash {
			return integrityf(packetID, row.Seq, "prev does not link to the preceding event",
				"prev %s, preceding hash %s", row.Prev, prevHash)
		}

		eventTime, err := ParseTime(row.Time)
		if err != nil {
			return integrityf(packetID, row.Seq, "stored event time is unreadable", "%v", err)
		}
		if i > 0 && eventTime.Before(prevTime) {
			return integrityf(packetID, row.Seq, "event time decreases",
				"%s precedes %s", row.Time, FormatTime(prevTime))
		}

		prevHash = row.Hash
		prevTime = eventTime
	}
	return nil
}

// checkColumn requires an indexed column to agree with the hashed payload it
// was derived from. A disagreement means one of the two was rewritten.
func checkColumn(packetID string, seq int64, field, column string, get func() (string, bool)) error {
	got, ok := get()
	if !ok {
		return integrityf(packetID, seq, "stored payload is missing a core field", "%s", field)
	}
	if got != column {
		return integrityf(packetID, seq, "stored column disagrees with the hashed payload",
			"%s: column %q, payload %q", field, column, got)
	}
	return nil
}

// Project returns a packet's current state.
//
// It refuses — with an *IntegrityError and no projection — when the chain does
// not verify, or when the stored state row disagrees with the chain it claims
// to be derived from. There is no path by which a packet in either condition
// is reported as valid.
func (l *Log) Project(ctx context.Context, packetID string) (*Projection, error) {
	if _, err := l.db.GetPacket(ctx, packetID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &RefusalError{
				PacketID:     packetID,
				Precondition: "packet exists",
				Needed:       "a packet id that has been created",
			}
		}
		return nil, err
	}

	if err := l.VerifyChain(ctx, packetID); err != nil {
		return nil, err
	}

	rows, err := l.db.ListEvents(ctx, packetID)
	if err != nil {
		return nil, err
	}

	stateRow, stateErr := l.db.GetPacketState(ctx, packetID)
	missingState := errors.Is(stateErr, sql.ErrNoRows)
	if stateErr != nil && !missingState {
		return nil, stateErr
	}

	if len(rows) == 0 {
		if !missingState {
			return nil, integrityf(packetID, 0, "state row exists for a packet with no events", "")
		}
		return &Projection{PacketID: packetID, LastHash: canonical.ZeroDigest, Events: []Event{}}, nil
	}
	if missingState {
		return nil, integrityf(packetID, 0, "packet has events but no state row", "")
	}

	tail := rows[len(rows)-1]
	if stateRow.LastSeq != tail.Seq {
		return nil, integrityf(packetID, tail.Seq, "state row is not derived from the chain tail",
			"state last_seq %d, chain tail seq %d", stateRow.LastSeq, tail.Seq)
	}
	if stateRow.LastHash != tail.Hash {
		return nil, integrityf(packetID, tail.Seq, "state row is not derived from the chain tail",
			"state last_hash %s, chain tail hash %s", stateRow.LastHash, tail.Hash)
	}
	if stateRow.StateSha256 != tail.StateSha256 {
		return nil, integrityf(packetID, tail.Seq, "state hash disagrees with the chain tail",
			"state row %s, event %s", stateRow.StateSha256, tail.StateSha256)
	}

	state, err := canonical.DecodeObject([]byte(stateRow.StateJson))
	if err != nil {
		return nil, integrityf(packetID, tail.Seq, "stored state is not decodable JSON", "%v", err)
	}
	stateBytes, err := canonical.Marshal(state)
	if err != nil {
		return nil, integrityf(packetID, tail.Seq, "stored state is not canonically encodable", "%v", err)
	}
	if string(stateBytes) != stateRow.StateJson {
		return nil, integrityf(packetID, tail.Seq, "stored state is not in canonical form", "")
	}
	if sum := canonical.SHA256Bytes(stateBytes); sum != stateRow.StateSha256 {
		return nil, integrityf(packetID, tail.Seq, "stored state does not hash to its recorded digest",
			"recomputed %s, stored %s", sum, stateRow.StateSha256)
	}

	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		obj, err := canonical.DecodeObject([]byte(row.Payload))
		if err != nil {
			return nil, integrityf(packetID, row.Seq, "stored payload is not decodable JSON", "%v", err)
		}
		events = append(events, Event(obj))
	}

	return &Projection{
		PacketID:    packetID,
		State:       state,
		StateSHA256: stateRow.StateSha256,
		LastSeq:     stateRow.LastSeq,
		LastHash:    stateRow.LastHash,
		Events:      events,
	}, nil
}

// VerifyPayloads checks a chain that already exists somewhere else — a native
// workflow-events.jsonl, say — without storing anything.
//
// It recomputes every hash, checks every link, and requires each line to be
// the exact canonical bytes it was hashed as. The decoded events are returned
// so a caller can summarise them; on failure it reports the first bad seq.
func VerifyPayloads(packetID string, payloads [][]byte) ([]Event, error) {
	out := make([]Event, 0, len(payloads))
	prevHash := canonical.ZeroDigest
	var prevTime time.Time

	for i, payload := range payloads {
		seq := int64(i + 1)

		obj, err := canonical.DecodeObject(payload)
		if err != nil {
			return nil, integrityf(packetID, seq, "line is not decodable JSON", "%v", err)
		}
		event := Event(obj)

		reencoded, err := event.Canonical()
		if err != nil {
			return nil, integrityf(packetID, seq, "line is not canonically encodable", "%v", err)
		}
		if string(reencoded) != string(payload) {
			return nil, integrityf(packetID, seq, "line is not in canonical form", "")
		}

		got, ok := event.Seq()
		if !ok || got != seq {
			return nil, integrityf(packetID, seq, "chain is not contiguous",
				"line %d carries seq %v", i+1, obj[FieldSeq])
		}

		stored, ok := event.Hash()
		if !ok {
			return nil, integrityf(packetID, seq, "event has no hash", "")
		}
		computed, err := event.ComputeHash()
		if err != nil {
			return nil, integrityf(packetID, seq, "event cannot be rehashed", "%v", err)
		}
		if computed != stored {
			return nil, integrityf(packetID, seq, "recomputed hash does not match the recorded hash",
				"recomputed %s, recorded %s", computed, stored)
		}

		prev, ok := event.Prev()
		if !ok || prev != prevHash {
			return nil, integrityf(packetID, seq, "prev does not link to the preceding event",
				"prev %v, preceding hash %s", obj[FieldPrev], prevHash)
		}

		eventTime, err := event.Time()
		if err != nil {
			return nil, integrityf(packetID, seq, "event time is unreadable", "%v", err)
		}
		if i > 0 && eventTime.Before(prevTime) {
			return nil, integrityf(packetID, seq, "event time decreases", "")
		}

		prevHash, prevTime = stored, eventTime
		out = append(out, event)
	}
	return out, nil
}
