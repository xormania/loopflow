// Package events implements the append-only per-packet event chain.
//
// Events carry a one-based seq, the previous event's hash in prev (64 zero
// hex characters at seq 1), an RFC3339 UTC time, a semantic state_sha256,
// and a hash over the canonical JSON of the event without its own hash field
// (decisions.md D6). Earlier events are never edited; a correction appends a
// superseding event.
//
// A broken chain, or a projection that disagrees with its events, blocks the
// packet as an evidence-integrity failure. It is never reported as valid.
//
// Phase 1 implements Append, VerifyChain, and Project here.
package events
