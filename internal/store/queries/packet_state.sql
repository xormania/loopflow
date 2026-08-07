-- The projected current state is replaced wholesale in the same transaction
-- that appends the event producing it. Events are never updated; this row is
-- a derived view and carries the seq and hash it was derived from so a
-- disagreement with the chain is detectable.
-- name: UpsertPacketState :exec
INSERT INTO packet_state (packet_id, state_json, state_sha256, last_seq, last_hash)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(packet_id) DO UPDATE SET
  state_json   = excluded.state_json,
  state_sha256 = excluded.state_sha256,
  last_seq     = excluded.last_seq,
  last_hash    = excluded.last_hash;

-- name: GetPacketState :one
SELECT * FROM packet_state WHERE packet_id = ?;
