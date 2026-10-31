-- name: ListExpiredRecordingsAfter :many
-- ListExpiredRecordingsAfter は保持期間ジョブ（docs/design.md「保持期間」）用。カメラごとの retention_days を ended_at が過ぎた録画を
-- (started_at, id) の順に、カーソル (after_started_at, after_id) より後から max_rows 件返す。
-- 削除に失敗して残った行を同じ実行の中で取り直さないよう、キーセットで進める。
SELECT recordings.* FROM recordings
JOIN cameras ON cameras.id = recordings.camera_id
WHERE recordings.ended_at < sqlc.arg(now)::timestamptz - make_interval(days => cameras.retention_days)
  AND (recordings.started_at, recordings.id) > (sqlc.arg(after_started_at)::timestamptz, sqlc.arg(after_id)::uuid)
ORDER BY recordings.started_at, recordings.id
LIMIT sqlc.arg(max_rows);
