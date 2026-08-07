-- 0003_claims.sql — who is working on what.
--
-- Peer harnesses pull work from the same store with no central coordinator, so
-- "I have this one" has to be a fact they can all see and none of them can win
-- twice. A claim is transient coordination, not evidence: it lives in its own
-- table rather than the event chain, so heartbeats do not bury the history a
-- packet is actually keeping.
--
-- expires_at makes a dead harness recoverable. Without it, a crashed process
-- would hold its packet forever and the loop would wedge with no way out that
-- is not a manual delete.

CREATE TABLE claims (
  packet_id   TEXT PRIMARY KEY REFERENCES packets(packet_id),
  owner       TEXT NOT NULL,
  note        TEXT NOT NULL DEFAULT '',
  acquired_at TEXT NOT NULL,
  expires_at  TEXT NOT NULL
);
