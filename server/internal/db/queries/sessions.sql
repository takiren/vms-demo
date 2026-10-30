-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetSessionByTokenHash :one
-- GetSessionByTokenHash は期限内のセッションだけを、ユーザーと一緒に返す。
SELECT sqlc.embed(sessions), sqlc.embed(users)
FROM sessions
JOIN users ON users.id = sessions.user_id
WHERE sessions.token_hash = $1
  AND sessions.expires_at > now();

-- name: TouchSession :execrows
-- TouchSession は最終アクセス時刻を更新し、期限を延ばす（スライディング）。
UPDATE sessions
SET last_seen_at = now(), expires_at = sqlc.arg(expires_at)
WHERE id = sqlc.arg(id)
  AND expires_at > now();

-- name: DeleteSessionByTokenHash :execrows
DELETE FROM sessions
WHERE token_hash = $1;

-- name: DeleteSessionsByUser :execrows
-- DeleteSessionsByUser はパスワードやロールを変えたときに、そのユーザーのセッションをすべて無効にする。
DELETE FROM sessions
WHERE user_id = $1;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions
WHERE expires_at <= now();
