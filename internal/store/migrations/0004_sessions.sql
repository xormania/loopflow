-- 0004_sessions.sql — which provider session is doing what, and is it alive.
--
-- There is no stable, cross-harness, process-readable worker id today: Codex
-- has a collaboration agent path and a rollout session id, Grok returns a
-- sessionId in its whole-message JSON, and the outer runner's numeric handle
-- is transient. Nothing correlates them, so before launching a replacement you
-- cannot cheaply answer "is the old one still alive?" -- and a replacement Grok
-- audit has already burned four turns re-reading sources the original had
-- already read. This table is that correlation.
--
-- packet_id deliberately has no foreign key. The packets these track are owned
-- by flow-workflow.py and live on disk; requiring `loopflow init` first would make
-- loopflow claim packets it does not own just to note who is working on one.
--
-- last_seen plus ttl_seconds gives the death timeout that does not otherwise
-- exist. A session past its ttl reports as stale, never as dead: loopflow did not
-- observe it terminate, and saying more than it knows is how a loop ends up
-- with two live workers.

CREATE TABLE sessions (
  packet_id   TEXT    NOT NULL,
  role        TEXT    NOT NULL,
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
  PRIMARY KEY (packet_id, role, cycle)
);

CREATE INDEX sessions_by_packet ON sessions(packet_id);
