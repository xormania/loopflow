-- 0002_packet_objective.sql — what a packet is for.
--
-- A packet's objective is the one thing you need when reading `wfc status`
-- and cannot derive from the event chain.

ALTER TABLE packets ADD COLUMN objective TEXT NOT NULL DEFAULT '';
