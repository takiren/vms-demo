-- name: CreateUser :one
INSERT INTO users (username, password_hash, role)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetUser :one
SELECT * FROM users
WHERE id = $1;

-- name: GetUserByUsername :one
-- GetUserByUsername は大文字小文字を区別しない（users_username_lower_key を使う）。
SELECT * FROM users
WHERE lower(username) = lower(sqlc.arg(username));

-- name: ListUsers :many
SELECT * FROM users
ORDER BY lower(username);

-- name: CountUsersByRole :one
SELECT count(*) FROM users
WHERE role = $1;

-- name: UpdateUserPassword :one
UPDATE users
SET password_hash = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateUserRole :one
UPDATE users
SET role = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteUser :execrows
DELETE FROM users
WHERE id = $1;

-- name: GetUserForUpdate :one
-- GetUserForUpdate はトランザクションの中で対象のユーザーの行をロックして返す。
SELECT * FROM users
WHERE id = $1
FOR UPDATE;

-- name: LockAdmins :many
-- LockAdmins は admin の行をすべてロックして ID を返す。最後の admin を消したり降格したりしないための
-- 判定に使う。同時に実行されたトランザクションは、先のトランザクションが終わるまで待ち、
-- その結果（降格・削除済みの行は除かれる）を見て判定する。デッドロックを避けるため ID の順にロックする。
SELECT id FROM users
WHERE role = 'admin'
ORDER BY id
FOR UPDATE;

-- name: RehashUserPassword :execrows
-- RehashUserPassword はログイン時にパラメーターの古いハッシュを作り直す。
-- 検証したハッシュがまだ保存されている場合だけ書き換える（同時に変更されたパスワードを上書きしない）。
UPDATE users
SET password_hash = sqlc.arg(new_hash)
WHERE id = sqlc.arg(id)
  AND password_hash = sqlc.arg(old_hash);

-- name: LockAdminBootstrap :exec
-- LockAdminBootstrap は初期 admin の作成をトランザクションの間だけ直列にする（advisory lock）。
SELECT pg_advisory_xact_lock(hashtextextended('vms-demo:bootstrap-admin', 0));
