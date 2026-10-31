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
