-- name: InsertRecording :one
INSERT INTO recordings (camera_id, started_at, ended_at, object_key, size_bytes, thumbnail_key)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetRecording :one
SELECT * FROM recordings
WHERE id = $1;

-- name: ListRecordingsInRange :many
-- ListRecordingsInRange は [range_start, range_end) と重なる録画を started_at の順に返す。
SELECT * FROM recordings
WHERE camera_id = sqlc.arg(camera_id)
  AND started_at < sqlc.arg(range_end)
  AND ended_at > sqlc.arg(range_start)
ORDER BY started_at;

-- name: ListExpiredRecordings :many
-- ListExpiredRecordings はカメラごとの retention_days を過ぎた録画を古い順に返す（保持期間ジョブ用）。
SELECT recordings.* FROM recordings
JOIN cameras ON cameras.id = recordings.camera_id
WHERE recordings.ended_at < sqlc.arg(now)::timestamptz - make_interval(days => cameras.retention_days)
ORDER BY recordings.started_at
LIMIT sqlc.arg(max_rows);

-- name: DeleteRecordings :execrows
-- DeleteRecordings は S3 からの削除に成功した分だけを渡す。
DELETE FROM recordings
WHERE id = ANY(sqlc.arg(ids)::uuid[]);
