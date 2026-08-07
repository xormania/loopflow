// Package artifacts is the content-addressed artifact store.
//
// Content lives at <state-root>/artifacts/sha256/<first2>/<digest> and its
// metadata lives in SQLite. The database never points at content whose digest
// was not verified: digests are computed on write and checked before content
// is served (decisions.md D5). No absolute path is stored in the database —
// the digest is the identity.
//
// Phase 1 implements Put, PutExpected, Get, and Stat here.
package artifacts
