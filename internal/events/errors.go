package events

import (
	"errors"
	"fmt"
	"strings"
)

// Classification values, kept as the strings the native tooling already uses
// (environment-facts.md 3c, verify-hosted-ci.py). The full taxonomy arrives
// with the lifecycle gates in Phase 5; these two are what Phase 1 can produce.
const (
	ClassEvidenceIntegrity = "evidence-integrity"
	ClassPrecondition      = "precondition-failed"
)

// ErrEvidenceIntegrity marks a failure of the stored evidence to hold
// together. Every IntegrityError matches it under errors.Is.
var ErrEvidenceIntegrity = errors.New(ClassEvidenceIntegrity)

// ErrPrecondition marks a refusal to act because the current state does not
// permit the requested action. Every RefusalError matches it under errors.Is.
var ErrPrecondition = errors.New(ClassPrecondition)

// IntegrityError reports that stored evidence does not verify. Seq identifies
// the first event at which the chain fails; it is 0 when the failure is not
// tied to a single event.
//
// A packet in this condition is blocked. Nothing derived from it may be
// reported as valid.
type IntegrityError struct {
	PacketID string
	Seq      int64
	Reason   string
	Detail   string
	err      error
}

func (e *IntegrityError) Error() string {
	var b strings.Builder
	b.WriteString(ClassEvidenceIntegrity)
	fmt.Fprintf(&b, ": packet %s", e.PacketID)
	if e.Seq > 0 {
		fmt.Fprintf(&b, " event %d", e.Seq)
	}
	fmt.Fprintf(&b, ": %s", e.Reason)
	if e.Detail != "" {
		fmt.Fprintf(&b, " (%s)", e.Detail)
	}
	return b.String()
}

// Classification reports the failure class this error belongs to.
func (e *IntegrityError) Classification() string { return ClassEvidenceIntegrity }

func (e *IntegrityError) Unwrap() []error {
	if e.err == nil {
		return []error{ErrEvidenceIntegrity}
	}
	return []error{ErrEvidenceIntegrity, e.err}
}

func integrityf(packetID string, seq int64, reason, format string, args ...any) *IntegrityError {
	return &IntegrityError{
		PacketID: packetID,
		Seq:      seq,
		Reason:   reason,
		Detail:   fmt.Sprintf(format, args...),
	}
}

// RefusalError reports that an action was refused because a precondition did
// not hold. It names the precondition, what was expected against what was
// found, and the evidence that would satisfy it — the cross-phase rule that
// every refusal is self-explaining (build-plan.md).
//
// A refusal is not an integrity failure: the stored evidence is intact and the
// caller may retry once the precondition holds.
type RefusalError struct {
	PacketID     string
	Precondition string
	Expected     string
	Actual       string
	Needed       string
	err          error
}

func (e *RefusalError) Error() string {
	var b strings.Builder
	b.WriteString(ClassPrecondition)
	fmt.Fprintf(&b, ": packet %s: %s", e.PacketID, e.Precondition)
	if e.Expected != "" || e.Actual != "" {
		fmt.Fprintf(&b, " (expected %s, got %s)", quoteOrNone(e.Expected), quoteOrNone(e.Actual))
	}
	if e.Needed != "" {
		fmt.Fprintf(&b, "; needed: %s", e.Needed)
	}
	return b.String()
}

// Classification reports the failure class this error belongs to.
func (e *RefusalError) Classification() string { return ClassPrecondition }

func (e *RefusalError) Unwrap() []error {
	if e.err == nil {
		return []error{ErrPrecondition}
	}
	return []error{ErrPrecondition, e.err}
}

func quoteOrNone(s string) string {
	if s == "" {
		return "none"
	}
	return fmt.Sprintf("%q", s)
}
