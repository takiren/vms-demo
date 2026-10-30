-- name: EnqueuePendingUpload :execrows
-- EnqueuePendingUpload は同じ local_path がすでにあれば何もしない（0 を返す）。
INSERT INTO pending_uploads (local_path)
VALUES ($1)
ON CONFLICT (local_path) DO NOTHING;

-- name: ClaimPendingUploads :many
-- ClaimPendingUploads は処理できる行を古い順に取り、next_attempt_at を lease_until に進めて返す。
-- 処理中に落ちても、リースが切れれば別のワーカーが拾い直す。
-- 返った行の next_attempt_at がリースの証明になる。MarkPendingUploadFailed と DeletePendingUpload に渡す。
UPDATE pending_uploads
SET next_attempt_at = sqlc.arg(lease_until)
WHERE id IN (
    SELECT p.id FROM pending_uploads p
    WHERE p.next_attempt_at <= now()
    ORDER BY p.created_at
    LIMIT sqlc.arg(max_rows)
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: MarkPendingUploadFailed :one
-- MarkPendingUploadFailed は失敗を記録し、次の試行時刻を設定する。
-- lease_until には ClaimPendingUploads が返した行の next_attempt_at をそのまま渡す。
-- リースが切れて別のワーカーが取り直していると一致せず、行を返さない（pgx.ErrNoRows）。
-- そのときはリースを失ったので、何もしないで手を引くこと。
UPDATE pending_uploads
SET attempts = attempts + 1,
    last_error = sqlc.arg(last_error),
    next_attempt_at = sqlc.arg(next_attempt_at)
WHERE id = sqlc.arg(id)
  AND next_attempt_at = sqlc.arg(lease_until)
RETURNING *;

-- name: DeletePendingUpload :execrows
-- DeletePendingUpload はアップロードと recordings への登録が済んだ行を消す。
-- lease_until は MarkPendingUploadFailed と同じく Claim が返した next_attempt_at。
-- 0 を返したらリースを失っている（別のワーカーが処理中）。
DELETE FROM pending_uploads
WHERE id = sqlc.arg(id)
  AND next_attempt_at = sqlc.arg(lease_until);

-- name: ListOldestPendingUploads :many
-- ListOldestPendingUploads はディスク逼迫時に古いものから捨てるために使う。
SELECT * FROM pending_uploads
ORDER BY created_at
LIMIT $1;
