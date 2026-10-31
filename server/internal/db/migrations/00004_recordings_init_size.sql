-- 録画再生（#13）。設計は docs/design.md の「録画再生の詳細」。
--
-- MediaMTX の fMP4 セグメントは 1 ファイルに init（ftyp + moov）とフラグメント（moof + mdat）が並ぶ。
-- HLS のプレイリストでは同じオブジェクトをバイト範囲で init（EXT-X-MAP）と本体に分けるので、
-- init の長さ（最初の moof の位置）を保存する。取り込みのときに求める。
-- NULL はこの列より前に取り込んだ行か、求められなかった行。再生のときに Garage から先頭を読んで埋める。

-- +goose Up

ALTER TABLE recordings
    ADD COLUMN init_size bigint CHECK (init_size > 0 AND init_size <= size_bytes);

-- +goose Down

ALTER TABLE recordings
    DROP COLUMN init_size;
