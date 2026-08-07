-- name: InsertEvent :exec
INSERT INTO events (packet_id, seq, hash, prev, time, payload, state_sha256)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- Chain tail. Returns sql.ErrNoRows for a packet with no events, which the
-- caller reads as "expect seq 1 with a zero prev".
-- name: GetChainTail :one
SELECT * FROM events WHERE packet_id = ? ORDER BY seq DESC LIMIT 1;

-- name: GetEvent :one
SELECT * FROM events WHERE packet_id = ? AND seq = ?;

-- name: ListEvents :many
SELECT * FROM events WHERE packet_id = ? ORDER BY seq;

-- name: CountEvents :one
SELECT COUNT(*) FROM events WHERE packet_id = ?;
