-- 録画取り込み（#11）の完了状態。設計は docs/design.md の「録画パイプライン」。
--
-- アップロードと recordings への登録が済んだ行は、すぐには消さずに uploaded_at を入れて残す。
-- next_attempt_at を uploaded_at + 猶予（最短 1 時間）にしておき、猶予が過ぎたらワーカーが
-- ローカルのファイルを消してから行を消す。行が残っている間は、ディレクトリ走査が同じファイルを
-- 積み直さない（local_path の一意制約）。
--
-- 消す前に、アップロードしたときのファイル（uploaded_size / uploaded_mtime）と今のファイル、
-- Garage のオブジェクト（HEAD のサイズ）が一致することを確かめる。違えば消さずにアップロードし直す
-- （書き込み中だった最新のセグメントを上げてしまった場合や、Garage がオブジェクトを失った場合）。

-- +goose Up

ALTER TABLE pending_uploads
    ADD COLUMN uploaded_at    timestamptz,
    -- アップロードしたときのファイルのサイズと mtime
    ADD COLUMN uploaded_size  bigint CHECK (uploaded_size >= 0),
    ADD COLUMN uploaded_mtime timestamptz,
    ADD CHECK ((uploaded_at IS NULL) = (uploaded_size IS NULL) AND (uploaded_at IS NULL) = (uploaded_mtime IS NULL));

-- ディスク逼迫時（#12）に、アップロード済み（消してよい）と未アップロードを分けて古い順に引く
CREATE INDEX pending_uploads_uploaded_created_idx ON pending_uploads ((uploaded_at IS NULL), created_at);

-- +goose Down

DROP INDEX pending_uploads_uploaded_created_idx;
ALTER TABLE pending_uploads
    DROP COLUMN uploaded_mtime,
    DROP COLUMN uploaded_size,
    DROP COLUMN uploaded_at;
