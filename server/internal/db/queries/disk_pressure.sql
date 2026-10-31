-- name: ClaimPendingUploadForDiskPressure :one
-- ClaimPendingUploadForDiskPressure は、ディスク逼迫時（docs/design.md「ディスク逼迫時の処理」）に消す候補を 1 行だけ古い順に取る。
-- uploaded が true ならアップロード済みの行、false なら未アップロードの行。
-- ワーカーの Claim と違い、まだ予定の時刻になっていない行（猶予中、バックオフ中）も取る。
-- ワーカーが処理中の行（leased_until が未来）と、exclude_paths（path ごとの最新のファイル）、
-- exclude_ids（この回で試した行）は飛ばす。
-- 取った行の next_attempt_at と leased_until を lease_until にし、取る前の next_attempt_at を
-- previous_next_attempt_at で返す。消さなかった行は RestorePendingUploadSchedule でそこに戻す。
-- 行がなければ pgx.ErrNoRows。exclude_paths と exclude_ids には nil ではなく空の配列を渡す（NULL だと何も返らない）。
WITH candidate AS (
    SELECT p.id, p.next_attempt_at
    FROM pending_uploads p
    WHERE (p.uploaded_at IS NOT NULL) = sqlc.arg(uploaded)::boolean
      AND (p.leased_until IS NULL OR p.leased_until <= now())
      AND p.local_path <> ALL(sqlc.arg(exclude_paths)::text[])
      AND p.id <> ALL(sqlc.arg(exclude_ids)::uuid[])
    ORDER BY p.created_at, p.id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
UPDATE pending_uploads
SET next_attempt_at = sqlc.arg(lease_until),
    leased_until = sqlc.arg(lease_until)
FROM candidate
WHERE pending_uploads.id = candidate.id
RETURNING pending_uploads.id, pending_uploads.local_path, pending_uploads.attempts, pending_uploads.last_error,
    pending_uploads.next_attempt_at, pending_uploads.created_at, pending_uploads.uploaded_at,
    pending_uploads.uploaded_size, pending_uploads.uploaded_mtime, pending_uploads.leased_until,
    candidate.next_attempt_at AS previous_next_attempt_at;

-- name: RestorePendingUploadSchedule :execrows
-- RestorePendingUploadSchedule は ClaimPendingUploadForDiskPressure で取ったが消さなかった行を、
-- 元の予定（next_attempt_at）に戻してリースを手放す。lease_until は取ったときの next_attempt_at。
UPDATE pending_uploads
SET next_attempt_at = sqlc.arg(next_attempt_at),
    leased_until = NULL
WHERE id = sqlc.arg(id)
  AND next_attempt_at = sqlc.arg(lease_until);
