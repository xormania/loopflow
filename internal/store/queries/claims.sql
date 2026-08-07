-- name: GetClaim :one
SELECT * FROM claims WHERE packet_id = ?;

-- name: PutClaim :exec
INSERT INTO claims (packet_id, owner, note, acquired_at, expires_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(packet_id) DO UPDATE SET
  owner       = excluded.owner,
  note        = excluded.note,
  acquired_at = excluded.acquired_at,
  expires_at  = excluded.expires_at;

-- name: DeleteClaim :exec
DELETE FROM claims WHERE packet_id = ?;

-- name: ListClaims :many
SELECT * FROM claims ORDER BY packet_id;

-- Packets nobody currently holds: never claimed, or claimed by a harness whose
-- lease has run out. Times are stored fixed-width so this string comparison is
-- a chronological one.
-- name: ListUnclaimedPackets :many
SELECT p.packet_id
FROM packets p
LEFT JOIN claims c ON c.packet_id = p.packet_id
WHERE c.packet_id IS NULL OR c.expires_at <= ?
ORDER BY p.created_at, p.packet_id;
