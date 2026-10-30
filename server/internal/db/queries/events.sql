-- name: InsertEvent :one
INSERT INTO events (kind, camera_id, payload)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListRecentEvents :many
SELECT * FROM events
ORDER BY created_at DESC, id DESC
LIMIT $1;
