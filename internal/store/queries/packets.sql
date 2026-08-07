-- name: CreatePacket :exec
INSERT INTO packets (packet_id, created_at) VALUES (?, ?);

-- name: GetPacket :one
SELECT * FROM packets WHERE packet_id = ?;

-- name: ListPackets :many
SELECT * FROM packets ORDER BY packet_id;
