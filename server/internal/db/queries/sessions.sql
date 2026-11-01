-- name: CreateSession :one
-- CreateSession はログイン時に検証したパスワードハッシュが変わっていない場合だけセッションを作る
-- （変わっていれば行を返さない）。ユーザーの行を FOR SHARE でロックするので、同時に実行された
-- パスワード変更（ハッシュの更新とセッションの全削除）とは直列になり、古いパスワードで作った
-- セッションが残らない。期限は DB の時計で now() + ttl にする。
INSERT INTO sessions (user_id, token_hash, expires_at)
SELECT users.id, sqlc.arg(token_hash)::bytea, now() + make_interval(secs => sqlc.arg(ttl_seconds)::float8)
FROM users
WHERE users.id = sqlc.arg(user_id)
  AND users.password_hash = sqlc.arg(password_hash)
FOR SHARE
RETURNING *;

-- name: GetSessionByTokenHash :one
-- GetSessionByTokenHash は期限内のセッションだけを、ユーザーと一緒に返す。
SELECT sqlc.embed(sessions), sqlc.embed(users)
FROM sessions
JOIN users ON users.id = sessions.user_id
WHERE sessions.token_hash = $1
  AND sessions.expires_at > now();

-- name: GetSessionByID :one
-- GetSessionByID は期限内のセッションを ID で、ユーザーと一緒に返す（ライブ用トークンの検証用。延長はしない）。
SELECT sqlc.embed(sessions), sqlc.embed(users)
FROM sessions
JOIN users ON users.id = sessions.user_id
WHERE sessions.id = $1
  AND sessions.expires_at > now();

-- name: TouchSession :execrows
-- TouchSession は最終アクセス時刻を更新し、期限を now() + idle に延ばす（スライディング）。
-- ただし作成時刻 + max_lifetime（絶対期限）は超えない。期限切れのセッションは延ばさない。
UPDATE sessions
SET last_seen_at = now(),
    expires_at = LEAST(
        now() + make_interval(secs => sqlc.arg(idle_seconds)::float8),
        created_at + make_interval(secs => sqlc.arg(max_lifetime_seconds)::float8))
WHERE id = sqlc.arg(id)
  AND expires_at > now();

-- name: DeleteSession :execrows
DELETE FROM sessions
WHERE id = $1;

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
