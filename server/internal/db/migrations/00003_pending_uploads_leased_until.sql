-- ディスク逼迫時の処理（#12）のためのリースの状態。設計は docs/design.md の「ディスク逼迫時の処理」。
--
-- next_attempt_at だけでは、ワーカーが処理中の行（リースの期限）と、猶予中やバックオフ中で待っている行の
-- 区別がつかない。ディスク逼迫時の処理は待っている行を取るが、処理中の行には触らない。
-- そのため、取得したときにリースの期限を leased_until にも入れ、処理を終えたら NULL に戻す。
-- 期限が過ぎた leased_until（ワーカーが落ちた）は、処理中ではないとみなす。

-- +goose Up

ALTER TABLE pending_uploads ADD COLUMN leased_until timestamptz;

-- +goose Down

ALTER TABLE pending_uploads DROP COLUMN leased_until;
