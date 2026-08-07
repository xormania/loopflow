-- 0005_attempts.sql — what was attempted, what happened, against what bindings.
--
-- A refused Flow transition raises before it appends anything, so the reason
-- exists only in the coordinator's transcript and is unrecoverable once that is
-- gone: the command has to be rerun to find out. This table is where a wrapper
-- puts it instead.
--
-- Append-only, and there are deliberately no UPDATE or DELETE queries against
-- it. An attempt is a fact about a moment; a later attempt is a new row.
--
-- Three things this schema is careful about, because conflating them would make
-- the record worse than nothing:
--
--   record_kind separates an attempt that changed nothing from an accepted
--   transition that recorded a failure. Both are "non-zero exit" or "outcome
--   failed" in casual speech and they mean completely different things.
--
--   exhaustiveness records that native validation is fail-fast. A refusal
--   names the FIRST failed precondition, never all of them, so this must never
--   be presented as a complete account of what blocks a packet.
--
--   the before/after bindings are what make a stored reason falsifiable later.
--   A reason is only the current explanation while the packet still has the
--   event count, head hash, and stage it had when the reason was produced.

CREATE TABLE attempts (
  attempt_id     TEXT PRIMARY KEY,
  packet_id      TEXT    NOT NULL,
  transition     TEXT    NOT NULL,
  argv           TEXT    NOT NULL,
  attempted_at   TEXT    NOT NULL,
  duration_ms    INTEGER NOT NULL,

  exit_code      INTEGER NOT NULL,
  marker         TEXT    NOT NULL DEFAULT '',
  reason         TEXT    NOT NULL DEFAULT '',
  record_kind    TEXT    NOT NULL,
  exhaustiveness TEXT    NOT NULL DEFAULT '',

  event_appended INTEGER NOT NULL,
  events_before  INTEGER NOT NULL,
  events_after   INTEGER NOT NULL,
  head_before    TEXT    NOT NULL DEFAULT '',
  head_after     TEXT    NOT NULL DEFAULT '',
  stage_before   TEXT    NOT NULL DEFAULT '',
  stage_after    TEXT    NOT NULL DEFAULT '',

  stdout_tail    TEXT    NOT NULL DEFAULT '',
  stderr_tail    TEXT    NOT NULL DEFAULT '',
  stdout_sha256  TEXT    NOT NULL DEFAULT '',
  stderr_sha256  TEXT    NOT NULL DEFAULT '',
  tool_sha256    TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX attempts_by_packet ON attempts(packet_id, attempted_at);
