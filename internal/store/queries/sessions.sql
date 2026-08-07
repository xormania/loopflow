-- name: GetWorkerSession :one
SELECT * FROM worker_sessions
WHERE packet_id = ? AND role = ? AND task = ? AND cycle = ?;

-- name: PutWorkerSession :exec
INSERT INTO worker_sessions (
  packet_id, role, task, cycle, client, session_id, agent_path, parent, pid,
  status, reason, note, started_at, last_seen, ttl_seconds
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(packet_id, role, task, cycle) DO UPDATE SET
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

-- name: ListWorkerSessions :many
SELECT * FROM worker_sessions ORDER BY packet_id, role, task, cycle;

-- name: ListWorkerSessionsForPacket :many
SELECT * FROM worker_sessions WHERE packet_id = ? ORDER BY role, task, cycle;

-- name: ArchiveWorkerSession :exec
INSERT INTO worker_session_history (
  packet_id, role, task, cycle, client, session_id, agent_path,
  status, liveness, started_at, last_seen, replaced_at, replaced_by, replace_note
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListWorkerSessionHistory :many
SELECT * FROM worker_session_history
WHERE packet_id = ? ORDER BY history_id;
