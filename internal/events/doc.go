// Package events implements the append-only per-packet event chain.
//
// An event is a JSON object carrying a one-based seq, the previous event's
// hash in prev (64 zero hex characters at seq 1), an RFC3339 UTC time, a
// semantic state_sha256, and a hash over the canonical JSON of the event
// without its own hash field (decisions.md D6). Phase-specific fields ride in
// the same object and are hashed like everything else, so an event is modelled
// as a map rather than a struct with an overflow bucket: the hashed object is
// the only representation, and there is nothing for a typed view to drift
// from.
//
// # What is stored, and why twice
//
// Appending writes two things in one transaction: the event, whose payload
// column holds the exact canonical bytes that were hashed, and the packet's
// current state, which carries the seq and hash it was derived from. The state
// row is a derived view kept for reading; the chain is the authority. Project
// re-derives from the chain and refuses to answer when the two disagree.
//
// # Corrections
//
// Earlier events are never edited. A correction appends a new event naming the
// claim it supersedes in supersedes_seq. Both events remain in the chain and
// both project; the superseded event's stored bytes are untouched, so its hash
// still verifies.
//
// # Refusals
//
// Two failure kinds are deliberately distinct. A RefusalError means the caller
// asked for something the current state does not permit — a stale seq, a
// forked prev, time running backwards — and names the precondition that failed
// and the evidence needed to satisfy it. An IntegrityError means the stored
// evidence itself does not hold together, and blocks the packet: it is never
// downgraded to a warning and never accompanied by a projection.
package events
