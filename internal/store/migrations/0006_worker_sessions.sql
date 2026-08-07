-- 0006_worker_sessions.sql — a wider key, and history that survives replacement.
--
-- The old key was (packet, role, cycle). That conflates genuinely different
-- role-tasks: gap-review, sensitivity-review, and final-audit are all auditor
-- custody at cycle 0, so one lingering record could block a later legitimate
-- audit, and a replacement could overwrite the wrong row. Custody role and
-- role-task kind are different facts and now occupy different columns.
--
-- cycle here is the caller's key for its own correction cycle. It is not Flow's
-- event cycle field, which counts something else — a failed gap review at event
-- seq 3 carries cycle 1 while its report is gap-cycle-0.md.
--
-- Rows are copied rather than dropped: an existing registration is somebody's
-- record of a live worker, and a schema change is not a reason to lose it.
-- Copied rows take the empty task, which is what they were registered under.

CREATE TABLE worker_sessions (
  packet_id   TEXT    NOT NULL,
  role        TEXT    NOT NULL,
  task        TEXT    NOT NULL DEFAULT '',
  cycle       INTEGER NOT NULL,
  client      TEXT    NOT NULL,
  session_id  TEXT    NOT NULL DEFAULT '',
  agent_path  TEXT    NOT NULL DEFAULT '',
  parent      TEXT    NOT NULL DEFAULT '',
  pid         INTEGER NOT NULL DEFAULT 0,
  status      TEXT    NOT NULL,
  reason      TEXT    NOT NULL DEFAULT '',
  note        TEXT    NOT NULL DEFAULT '',
  started_at  TEXT    NOT NULL,
  last_seen   TEXT    NOT NULL,
  ttl_seconds INTEGER NOT NULL,
  PRIMARY KEY (packet_id, role, task, cycle)
);

INSERT INTO worker_sessions (
  packet_id, role, task, cycle, client, session_id, agent_path, parent, pid,
  status, reason, note, started_at, last_seen, ttl_seconds
)
SELECT packet_id, role, '', cycle, client, session_id, agent_path, parent, pid,
       status, reason, note, started_at, last_seen, ttl_seconds
FROM sessions;

DROP TABLE sessions;

CREATE INDEX worker_sessions_by_packet ON worker_sessions(packet_id);

-- Replacing a registration displaces somebody's record of a worker. That has to
-- be attributable, so the displaced row is kept with who displaced it and why.
CREATE TABLE worker_session_history (
  history_id   INTEGER PRIMARY KEY AUTOINCREMENT,
  packet_id    TEXT    NOT NULL,
  role         TEXT    NOT NULL,
  task         TEXT    NOT NULL DEFAULT '',
  cycle        INTEGER NOT NULL,
  client       TEXT    NOT NULL,
  session_id   TEXT    NOT NULL DEFAULT '',
  agent_path   TEXT    NOT NULL DEFAULT '',
  status       TEXT    NOT NULL,
  liveness     TEXT    NOT NULL,
  started_at   TEXT    NOT NULL,
  last_seen    TEXT    NOT NULL,
  replaced_at  TEXT    NOT NULL,
  replaced_by  TEXT    NOT NULL DEFAULT '',
  replace_note TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX worker_session_history_by_packet ON worker_session_history(packet_id);
