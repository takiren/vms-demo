-- name: CreateCamera :one
INSERT INTO cameras (name, main_url, sub_url, credentials_enc, retention_days, enabled)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetCamera :one
SELECT * FROM cameras
WHERE id = $1;

-- name: ListCameras :many
SELECT * FROM cameras
ORDER BY name;

-- name: ListEnabledCameras :many
SELECT * FROM cameras
WHERE enabled
ORDER BY name;

-- name: UpdateCamera :one
UPDATE cameras
SET name = $2,
    main_url = $3,
    sub_url = $4,
    credentials_enc = $5,
    retention_days = $6,
    enabled = $7,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteCamera :execrows
-- DeleteCamera は録画が残っていると外部キー（ON DELETE RESTRICT、SQLSTATE 23001）で失敗する。
DELETE FROM cameras
WHERE id = $1;

-- name: CreateCameraWithID :one
-- 認証情報の暗号文を ID に結び付ける（AAD）ため、ID を先に Go で決めて（UUIDv7）挿入する。
INSERT INTO cameras (id, name, main_url, sub_url, credentials_enc, retention_days, enabled)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetCameraForUpdate :one
SELECT * FROM cameras
WHERE id = $1
FOR UPDATE;
