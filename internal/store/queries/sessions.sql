-- name: GetSession :one
SELECT * FROM sessions WHERE packet_id = ? AND role = ? AND cycle = ?;

-- name: PutSession :exec
INSERT INTO sessions (
  packet_id, role, cycle, client, session_id, agent_path, parent, pid,
  status, reason, note, started_at, last_seen, ttl_seconds
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(packet_id, role, cycle) DO UPDATE SET
  client      = excluded.client,
  session_id  = excluded.session_id,
  agent_path  = excluded.agent_path,
  parent      = excluded.parent,
  pid         = excluded.pid,
  status      = excluded.status,
  reason      = excluded.reason,
  note        = excluded.note,
  started_at  = excluded.started_at,
  last_seen   = excluded.last_seen,
  ttl_seconds = excluded.ttl_seconds;

-- name: ListSessions :many
SELECT * FROM sessions ORDER BY packet_id, role, cycle;

-- name: ListSessionsForPacket :many
SELECT * FROM sessions WHERE packet_id = ? ORDER BY role, cycle;
