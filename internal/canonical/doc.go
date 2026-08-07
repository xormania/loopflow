// Package canonical produces the canonical JSON byte form used for every
// hash in the control plane, and the SHA-256 helpers over it.
//
// The encoding must reproduce, byte for byte, the native Python form
// json.dumps(obj, sort_keys=True, separators=(",", ":")) that the existing
// event chain was written with (decisions.md D6). Its fidelity is proven by
// the golden vector in phase-1-foundation.md; when the two disagree the
// encoder is wrong, never the vector.
//
// Phase 1 implements Marshal, SHA256Hex, and a raw-bytes digest helper here.
package canonical
