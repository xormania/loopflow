// Package canonical produces the canonical JSON byte form used for every hash
// in the control plane, and the SHA-256 helpers over it.
//
// The encoding reproduces, byte for byte, the native Python form
//
//	json.dumps(obj, sort_keys=True, separators=(",", ":"))
//
// that the existing event chain was written with (decisions.md D6). Its
// fidelity is proven by the golden vector in canonical_test.go and by fixtures
// generated from CPython in testdata/. When Go and the vector disagree, the
// encoder is wrong — never the vector.
//
// # Value model
//
// Marshal deliberately accepts only the JSON value model:
//
//	nil, bool, string, json.Number, Go integer kinds, []any, map[string]any
//
// plus any named type or concrete container whose shape reduces to that —
// events.Event, map[string]string, []int, and so on.
//
// Four things are refused rather than guessed at, because in each case the Go
// value does not say which JSON document was meant:
//
//   - structs, since there is no single obvious mapping from Go fields to the
//     object Python would have hashed, and a wrong guess produces a
//     valid-looking hash over the wrong bytes;
//   - floats, since Python and Go disagree on their shortest representation
//     (phase-1-foundation.md requires rejecting rather than formatting);
//   - byte slices, which encoding/json base64-encodes and a generic array walk
//     would render as numbers; and
//   - typed nil maps and slices, which encoding/json writes as null while an
//     empty literal writes {} or []. Write an untyped nil for null.
//
// This means encoding/json.Unmarshal is the wrong way to obtain a value for
// Marshal: it decodes every number to float64, which Marshal rejects. Use
// Decode instead, which preserves number literals exactly.
//
// # Fail-closed decoding
//
// Decode is stricter than encoding/json in two ways that matter for evidence
// integrity, because both are silent corruptions in the standard decoder:
//
//   - duplicate object keys are rejected rather than last-one-wins, since the
//     document's meaning would otherwise depend on the parser; and
//   - lone surrogate escapes are rejected rather than replaced with U+FFFD,
//     since a replaced character rehashes to a different digest than the bytes
//     that were actually written.
package canonical
