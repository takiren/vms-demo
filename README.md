# vms-demo

セルフホストで動かす小規模な VMS（Video Management System）です。

現在は設計段階で、まだ実装はありません。

- サーバー 1 台で、最大 32 台程度の RTSP カメラを常時録画する
- ライブは WebRTC / LL-HLS、録画再生は HLS
- 録画は S3 互換ストレージ（Garage）に保存する

## 技術スタック

- 取り込み・配信: MediaMTX
- API: Go（Echo v5、pgx、sqlc、goose、oapi-codegen）
- ストレージ: Garage
- DB: PostgreSQL
- リバースプロキシ: Caddy
- メディア処理: ffmpeg（LGPL ビルド）
- フロントエンド: Vite、React、TanStack Router、TanStack Query、Tailwind CSS
- 起動: docker compose

詳細は [docs/design.md](docs/design.md) を参照してください。

## ライセンス

Apache-2.0（予定）
