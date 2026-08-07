-- Append-only. There is deliberately no update or delete here: an attempt is a
-- fact about a moment, and a later attempt is a new row.

-- name: InsertAttempt :exec
INSERT INTO attempts (
  attempt_id, packet_id, transition, argv, attempted_at, duration_ms,
  exit_code, marker, reason, record_kind, exhaustiveness,
  event_appended, events_before, events_after,
  head_before, head_after, stage_before, stage_after,
  stdout_tail, stderr_tail, stdout_sha256, stderr_sha256, tool_sha256
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListAttempts :many
SELECT * FROM attempts WHERE packet_id = ? ORDER BY attempted_at, attempt_id;

-- name: ListAllAttempts :many
SELECT * FROM attempts ORDER BY packet_id, attempted_at, attempt_id;
