-- Content is immutable and named by its digest, so a repeated put of
-- identical bytes is a no-op rather than a conflict.
-- name: InsertArtifact :exec
INSERT INTO artifacts (digest, size, media_type, class, created_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(digest) DO NOTHING;

-- name: GetArtifact :one
SELECT * FROM artifacts WHERE digest = ?;

-- name: ListArtifacts :many
SELECT * FROM artifacts ORDER BY created_at, digest;
