-- 0001_foundation.sql — Phase 1 foundation schema.
--
-- Applied once, inside a transaction, and recorded in schema_migrations.
-- Later phases add migrations; this file is extended only by adding new
-- numbered files beside it, never by rewriting what has already been applied.
--
-- events.payload stores the exact canonical bytes that were hashed, including
-- the event's own hash field. Verification and projection re-parse from
-- payload so the database cannot drift from the hashed form.

CREATE TABLE packets (
  packet_id   TEXT PRIMARY KEY,
  created_at  TEXT NOT NULL
);

CREATE TABLE events (
  packet_id     TEXT NOT NULL REFERENCES packets(packet_id),
  seq           INTEGER NOT NULL,
  hash          TEXT NOT NULL,
  prev          TEXT NOT NULL,
  time          TEXT NOT NULL,
  payload       TEXT NOT NULL,   -- full canonical event JSON, incl. hash
  state_sha256  TEXT NOT NULL,
  PRIMARY KEY (packet_id, seq)
);

CREATE TABLE packet_state (
  packet_id     TEXT PRIMARY KEY REFERENCES packets(packet_id),
  state_json    TEXT NOT NULL,
  state_sha256  TEXT NOT NULL,
  last_seq      INTEGER NOT NULL,
  last_hash     TEXT NOT NULL
);

CREATE TABLE artifacts (
  digest      TEXT PRIMARY KEY,          -- lowercase hex sha256
  size        INTEGER NOT NULL,
  media_type  TEXT NOT NULL,
  class       TEXT NOT NULL,
  created_at  TEXT NOT NULL
);
