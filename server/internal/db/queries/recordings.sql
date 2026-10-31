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

-- name: UpsertRecording :one
-- UpsertRecording は取り込みのリトライで同じセグメントを登録し直しても 1 行にする（object_key で一意）。
-- 既存の行と camera_id / started_at が違う（別のセグメントがキーで衝突した）ときは更新せず、行を返さない
-- （pgx.ErrNoRows）。サムネイルの生成に失敗したリトライでは、前回のサムネイルを残す。
-- init_size（#13）は求められなかったとき NULL で、そのときは前回の値を残さない（ファイルが変わっている）。
INSERT INTO recordings (camera_id, started_at, ended_at, object_key, size_bytes, thumbnail_key, init_size)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (object_key) DO UPDATE
SET ended_at = EXCLUDED.ended_at,
    size_bytes = EXCLUDED.size_bytes,
    thumbnail_key = COALESCE(EXCLUDED.thumbnail_key, recordings.thumbnail_key),
    init_size = EXCLUDED.init_size
WHERE recordings.camera_id = EXCLUDED.camera_id
  AND recordings.started_at = EXCLUDED.started_at
RETURNING *;
