-- 録画再生（#13）と切り出し（#14）のクエリ。

-- name: ListRecordingsOverlapping :many
-- ListRecordingsOverlapping は [range_start, range_end) と重なる録画を started_at の順に最大 max_rows 件返す。
-- 呼び出し側は max_rows を上限 + 1 にして、超えたかどうかを判定する。
SELECT * FROM recordings
WHERE camera_id = sqlc.arg(camera_id)
  AND started_at < sqlc.arg(range_end)
  AND ended_at > sqlc.arg(range_start)
ORDER BY started_at, id
LIMIT sqlc.arg(max_rows);

-- name: SetRecordingInitSize :execrows
-- SetRecordingInitSize は再生のときに求めた init の長さを保存する。オブジェクトが変わっていない
-- （size_bytes が同じ）ときだけ更新する。
UPDATE recordings
SET init_size = sqlc.arg(init_size)
WHERE id = sqlc.arg(id)
  AND size_bytes = sqlc.arg(size_bytes)
  AND init_size IS NULL;
