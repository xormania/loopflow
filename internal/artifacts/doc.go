// Package artifacts is the content-addressed artifact store.
//
// Content lives at <root>/sha256/<first2>/<digest> and its metadata lives in
// SQLite. The digest is the identity: no absolute path is ever stored in the
// database, and no API response exposes the layout (decisions.md D5).
//
// # Nothing unverified is admitted or served
//
// Every byte that enters the store is hashed as it is written, and the digest
// is recomputed before any byte leaves. A recorded artifact whose bytes no
// longer hash to its digest is an evidence-integrity failure: Get reports it
// and serves nothing. Verification happens on a separate pass before the
// content handle is returned, rather than while streaming to the caller,
// because a reader that discovers corruption at EOF has already served the
// corrupt bytes.
//
// The cost is that Get reads each artifact twice. That is the deliberate
// trade: this store holds the evidence that gates merges, and Phase 1 has no
// throughput requirement that would justify serving unverified bytes. The
// verify-once-with-a-size-and-mtime-guard alternative offered by
// phase-1-foundation.md is not taken.
//
// # Immutability
//
// Content is written to a temporary file in the store root, hashed, and
// renamed into place read-only. A digest that is already present is deduped
// after confirming the stored bytes are identical; a mismatch there means the
// stored copy is corrupt, since the incoming copy hashed to the digest by
// construction.
package artifacts
