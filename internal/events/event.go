package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/xormania/wfc/internal/canonical"
)

// Core event field names. Phase-specific fields are not enumerated: they ride
// in the same object, are hashed identically, and are preserved verbatim.
const (
	FieldSeq           = "seq"
	FieldPrev          = "prev"
	FieldTime          = "time"
	FieldPhase         = "phase"
	FieldOutcome       = "outcome"
	FieldIssueKey      = "issue_key"
	FieldStateSHA256   = "state_sha256"
	FieldHash          = "hash"
	FieldSupersedesSeq = "supersedes_seq"
)

// TimeFormat is the wire form of an event's time: RFC3339 with exactly six
// fractional digits and a literal Z, matching the native events recorded in
// environment-facts.md 3c. The time is hashed as a string, so it must have
// exactly one spelling.
const TimeFormat = "2006-01-02T15:04:05.000000Z"

// Event is a complete event object. Values are drawn from the canonical value
// model: numbers decoded from storage are json.Number, and callers may supply
// Go integers.
type Event map[string]any

// Clone returns a shallow copy. Appending never mutates a caller's map.
func (e Event) Clone() Event {
	out := make(Event, len(e)+4)
	for k, v := range e {
		out[k] = v
	}
	return out
}

// withoutHash returns a copy with the hash field removed — the object the
// hash is computed over.
func (e Event) withoutHash() Event {
	out := make(Event, len(e))
	for k, v := range e {
		if k == FieldHash {
			continue
		}
		out[k] = v
	}
	return out
}

// ComputeHash returns the SHA-256 of the canonical JSON of e without its own
// hash field (decisions.md D6).
func (e Event) ComputeHash() (string, error) {
	return canonical.SHA256Hex(map[string]any(e.withoutHash()))
}

// Canonical returns the canonical bytes of the complete event, hash included.
// These are the bytes stored in the payload column, so that verification and
// projection re-parse exactly what was hashed.
func (e Event) Canonical() ([]byte, error) {
	return canonical.Marshal(map[string]any(e))
}

// Seq returns the event's sequence number.
func (e Event) Seq() (int64, bool) { return asInt64(e[FieldSeq]) }

// SupersedesSeq returns the sequence number this event corrects, if any.
func (e Event) SupersedesSeq() (int64, bool) {
	v, present := e[FieldSupersedesSeq]
	if !present {
		return 0, false
	}
	return asInt64(v)
}

// Str returns a string-valued field.
func (e Event) Str(field string) (string, bool) {
	s, ok := e[field].(string)
	return s, ok
}

// Prev returns the previous event's hash.
func (e Event) Prev() (string, bool) { return e.Str(FieldPrev) }

// Hash returns the event's own hash.
func (e Event) Hash() (string, bool) { return e.Str(FieldHash) }

// StateSHA256 returns the semantic hash of the state this event produced.
func (e Event) StateSHA256() (string, bool) { return e.Str(FieldStateSHA256) }

// Time returns the event's parsed time.
func (e Event) Time() (time.Time, error) {
	raw, ok := e.Str(FieldTime)
	if !ok {
		return time.Time{}, fmt.Errorf("events: %s is missing or not a string", FieldTime)
	}
	return ParseTime(raw)
}

// ParseTime parses an event time. UTC must be spelled with a trailing Z: the
// value is hashed as a string, so an equivalent offset such as +00:00 would be
// a second spelling of the same instant and a second hash.
func ParseTime(raw string) (time.Time, error) {
	if len(raw) == 0 || raw[len(raw)-1] != 'Z' {
		return time.Time{}, fmt.Errorf("events: time %q is not UTC spelled with a trailing Z", raw)
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("events: time %q is not RFC3339: %w", raw, err)
	}
	return t.UTC(), nil
}

// FormatTime renders t in the wire form.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// asInt64 accepts the integer spellings an event may carry: json.Number when
// it was decoded from storage, and Go's integer kinds when a caller built it
// in memory.
func asInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			return 0, false
		}
		return n, true
	case int:
		return int64(x), true
	case int8:
		return int64(x), true
	case int16:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case uint:
		return int64(x), true
	case uint8:
		return int64(x), true
	case uint16:
		return int64(x), true
	case uint32:
		return int64(x), true
	case uint64:
		if x > 1<<62 {
			return 0, false
		}
		return int64(x), true
	default:
		return 0, false
	}
}
