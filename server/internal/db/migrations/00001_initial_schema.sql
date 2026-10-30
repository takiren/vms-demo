-- 初期スキーマ。設計は docs/design.md の「DB（PostgreSQL）」。
--
-- - ID はすべて UUIDv7。PostgreSQL 18 の uuidv7() を既定値にする（Go 側で決めて渡してもよい）
-- - 時刻はすべて timestamptz
-- - updated_at はトリガーではなく、更新するクエリの中で now() を入れる

-- +goose Up

CREATE TABLE users (
    id            uuid        PRIMARY KEY DEFAULT uuidv7(),
    -- ASCII に限る。大文字小文字を区別しない一意性は users_username_lower_key で保証する
    username      text        NOT NULL CHECK (username ~ '^[A-Za-z0-9._-]{1,64}$'),
    -- argon2id の PHC 文字列（$argon2id$v=19$m=...,t=...,p=...$salt$hash）
    password_hash text        NOT NULL CHECK (password_hash LIKE '$argon2id$%'),
    role          text        NOT NULL CHECK (role IN ('admin', 'viewer')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX users_username_lower_key ON users (lower(username));

CREATE TABLE sessions (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- セッショントークンの SHA-256。生のトークンは保存しない
    token_hash   bytea       NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at)
);

CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

CREATE TABLE cameras (
    id              uuid        PRIMARY KEY DEFAULT uuidv7(),
    name            text        NOT NULL UNIQUE CHECK (char_length(name) BETWEEN 1 AND 100),
    -- 認証情報を含まない URL。認証情報は credentials_enc に暗号化して持つ。
    -- CHECK は scheme:// の後の authority（最初の / ? # まで）に @ があるもの（userinfo）を拒否する
    main_url        text        NOT NULL CHECK (main_url ~ '^[A-Za-z][A-Za-z0-9+.-]*://[^/?#@]+([/?#].*)?$'),
    -- 多分割ライブ用（H.264）。ないカメラもある
    sub_url         text        CHECK (sub_url ~ '^[A-Za-z][A-Za-z0-9+.-]*://[^/?#@]+([/?#].*)?$'),
    -- AES-256-GCM で暗号化した RTSP の認証情報（鍵は VMS_SECRET_KEY）
    credentials_enc bytea,
    retention_days  integer     NOT NULL DEFAULT 7 CHECK (retention_days > 0),
    enabled         boolean     NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE recordings (
    id            uuid        PRIMARY KEY DEFAULT uuidv7(),
    -- RESTRICT: 録画が残っているカメラは消せない。カメラを消すときは、先に録画（S3 のオブジェクト）を
    -- 消す必要がある。CASCADE だと Garage にオブジェクトが残ったまま目録だけが消える
    camera_id     uuid        NOT NULL REFERENCES cameras (id) ON DELETE RESTRICT,
    started_at    timestamptz NOT NULL,
    ended_at      timestamptz NOT NULL,
    object_key    text        NOT NULL UNIQUE CHECK (object_key <> ''),
    size_bytes    bigint      NOT NULL CHECK (size_bytes >= 0),
    thumbnail_key text        CHECK (thumbnail_key <> ''),
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (ended_at >= started_at)
);

CREATE INDEX recordings_camera_id_started_at_idx ON recordings (camera_id, started_at);

CREATE TABLE pending_uploads (
    id              uuid        PRIMARY KEY DEFAULT uuidv7(),
    -- 共有ボリューム上の fMP4 のパス
    local_path      text        NOT NULL UNIQUE CHECK (local_path <> ''),
    attempts        integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error      text,
    -- これより前には処理しない。処理中の行は、この時刻をリース期限として先に進める
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX pending_uploads_next_attempt_at_idx ON pending_uploads (next_attempt_at);
CREATE INDEX pending_uploads_created_at_idx ON pending_uploads (created_at);

CREATE TABLE events (
    id         uuid        PRIMARY KEY DEFAULT uuidv7(),
    -- disk_pressure、upload_failed、camera_disconnected など
    kind       text        NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_]{0,63}$'),
    -- SET NULL: イベントは記録なので、カメラを消しても残す
    camera_id  uuid        REFERENCES cameras (id) ON DELETE SET NULL,
    payload    jsonb       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(payload) = 'object'),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX events_created_at_idx ON events (created_at);
CREATE INDEX events_camera_id_idx ON events (camera_id);

-- +goose Down

DROP TABLE events;
DROP TABLE pending_uploads;
DROP TABLE recordings;
DROP TABLE cameras;
DROP TABLE sessions;
DROP TABLE users;
