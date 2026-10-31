# server

Go による API サーバーの実装ディレクトリです。

- `cmd/api`: API サーバーのエントリポイント
- `internal/config`: 環境変数からの設定の読み込みと検証
- `internal/api`: ルーティング、ハンドラ、ミドルウェア（CSRF、認証・認可）
  - `gen/`: `api/openapi.yaml` から oapi-codegen で生成したコード（手で編集しない）
  - `apierror/`: 共通エラー形式への変換
  - `validation/`: OpenAPI によるリクエスト検証のミドルウェア
  - `authz/`: OpenAPI の `security` と `x-min-role` から作る、操作ごとの認可の表
  - `clientip/`: 信頼するプロキシの `X-Forwarded-For` から接続元 IP を求める
- `internal/auth`: ユーザー、パスワード（argon2id）、セッション、ログインの回数制限
- `internal/camera`: カメラの登録と検証、RTSP 認証情報の暗号化、MediaMTX に渡す source の組み立て
- `internal/mediamtx`: MediaMTX の Control API のクライアントと、DB のカメラを path に反映する reconciler
- `internal/sealbox`: AES-256-GCM による秘密情報の暗号化（DB に保存する認証情報など）
- `internal/testutil/pgtest`: 結合テスト用に PostgreSQL のコンテナとテストごとの DB を用意する
- `internal/testutil/garagetest`: 結合テスト用に Garage のコンテナ（layout、キー、バケット）を用意する
- `internal/s3store`: Garage（S3）のクライアント。アップロード、削除（DeleteObjects）、presigned URL
- `internal/ingest`: 録画の取り込み（フック、ディレクトリ走査、サムネイル、アップロード、ローカルの削除、ディスク逼迫時の処理）
- `internal/retention`: 保持期間ジョブ（期限切れの録画を Garage と目録から消す）
- `internal/db`: PostgreSQL の接続プールとマイグレーション
  - `migrations/`: goose の SQL マイグレーション（バイナリに埋め込み、API の起動時に適用する）
  - `queries/`: sqlc のクエリ
  - `sqlc/`: `queries/` から sqlc で生成したコード（手で編集しない）
- `internal/`: そのほかのビジネスロジック、カメラ管理、録画パイプライン、DB アクセスなど

## 開発

Go 1.27 が必要です。ツール（oapi-codegen、sqlc、air、golangci-lint）はバージョンを固定して `go tool` / `go run` で動かすので、別途インストールは要りません。

```sh
cd server
make generate   # api/openapi.yaml と internal/db の SQL からコードを生成する
make build      # go build ./...
make test       # go test -race（testcontainers の結合テストを含む。Docker が必要。カバレッジは coverage.out）
make test-short # Docker を使わないユニットテストだけ（go test -short）
make lint       # golangci-lint
make dev        # air でホットリロードしながら起動する（../.env を読む）
```

`api/openapi.yaml`、`internal/db/migrations/`、`internal/db/queries/` を変えたら `make generate` して、生成物もコミットしてください。CI で差分を確認しています。sqlc は cgo を使うので、初回の `make generate` はビルドに少し時間がかかります。

## DB

- API は起動時にマイグレーションを適用する（失敗したら起動しない）。goose のセッションロック（PostgreSQL の advisory lock）を取るので、複数のプロセスが同時に起動しても安全
- マイグレーションを追加するときは `internal/db/migrations/` に `000NN_<name>.sql` を足す（`-- +goose Up` と `-- +goose Down` の両方を書く）。適用済みのファイルは書き換えない
- PostgreSQL 18 以上が必要（ID の既定値に `uuidv7()` を使う）
- 結合テストは `postgres:18.6-alpine` を testcontainers で起動する。テストごとに別のデータベースを作る

## 設定（環境変数）

| 変数 | 必須 | 既定値 | 説明 |
|---|---|---|---|
| `DATABASE_URL` | ○ | | PostgreSQL の接続 URL（`postgres://` か `postgresql://`） |
| `VMS_SECRET_KEY` | ○ | | 32 バイトの鍵を base64 にしたもの（`openssl rand -base64 32`） |
| `VMS_LISTEN_ADDR` | | `:8080` | 待ち受けアドレス |
| `VMS_LOG_LEVEL` | | `info` | `debug` / `info` / `warn` / `error` |
| `VMS_SHUTDOWN_TIMEOUT` | | `10s` | SIGINT / SIGTERM の後、処理中のリクエストを待つ時間 |
| `VMS_HOST` | ○ | | ブラウザが使うホスト名（例: `vms.localhost`）。公開オリジンは `https://<VMS_HOST>` |
| `VMS_PUBLIC_ORIGIN` | | `https://<VMS_HOST>` | 公開オリジンを上書きする（ポートが 443 以外のときなど）。https のみ |
| `VMS_TRUSTED_PROXIES` | | なし | `X-Forwarded-For` を信用する直接の接続元（IP か CIDR のカンマ区切り）。Caddy のアドレスを書く |
| `VMS_SESSION_IDLE_TIMEOUT` | | `12h` | 最後のアクセスからセッションが切れるまで（1m 以上） |
| `VMS_SESSION_MAX_LIFETIME` | | `168h` | ログインからセッションが必ず切れるまで（アイドル以上、8760h 以下） |
| `VMS_ADMIN_USERNAME` | | | 初期 admin のユーザー名。`VMS_ADMIN_PASSWORD` と組で設定する |
| `VMS_ADMIN_PASSWORD` | | | 初期 admin のパスワード（12〜128 文字）。admin が 1 人もいないときだけ使う |
| `VMS_MEDIAMTX_API_URL` | | `http://127.0.0.1:9997` | MediaMTX の Control API（http / https、パスなし） |
| `VMS_MEDIAMTX_API_USER` | | `vms-api` | Control API に名乗るユーザー名（`[A-Za-z0-9._-]`、64 文字まで） |
| `VMS_MEDIAMTX_API_PASSWORD` | ○ | | Control API に名乗るパスワード（16〜256 バイト。`openssl rand -hex 32`） |
| `VMS_MEDIAMTX_SYNC_INTERVAL` | | `30s` | カメラを MediaMTX に再同期する間隔（5s〜1h） |
| `S3_ENDPOINT` | △ | | Garage の公開エンドポイント（`https://s3.<host>`）。presigned URL の署名に使う。https のみ、パスなし |
| `S3_INTERNAL_ENDPOINT` | | `S3_ENDPOINT` | API 自身が Garage に接続する先（全部コンテナなら `http://garage:3900`） |
| `S3_REGION` | | `garage` | Garage の `s3_region` |
| `S3_BUCKET` | | `recordings` | バケット |
| `S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY` | △ | | API の Garage のキー。`S3_*` はどれかを設定したら全部必須 |
| `VMS_RECORDINGS_DIR` | | | 録画の取り込みを有効にする。API から見た録画ディレクトリ（相対パスは作業ディレクトリから）。設定すると `S3_*` と `VMS_HOOK_TOKEN` が必須 |
| `VMS_MEDIAMTX_RECORDINGS_DIR` | | `/recordings` | MediaMTX から見た録画ディレクトリ（通知の `segment_path` の先頭） |
| `VMS_RECORDINGS_TIMEZONE` | | `UTC` | MediaMTX がファイル名を書くタイムゾーン（MediaMTX の TZ） |
| `VMS_HOOK_TOKEN` | △ | | MediaMTX のフックの共有シークレット（32 文字以上）。未設定ならフックは 503 |
| `VMS_FFMPEG_PATH` / `VMS_FFPROBE_PATH` | | `ffmpeg` / `ffprobe` | LGPL ビルドの ffmpeg / ffprobe（起動時に存在を確かめる） |
| `VMS_INGEST_WORKERS` | | `2` | アップロードのワーカー数（1〜16） |
| `VMS_INGEST_SCAN_INTERVAL` | | `5m` | 取りこぼしを拾うディレクトリ走査の間隔（10s 以上） |
| `VMS_INGEST_DELETE_GRACE` | | `1h` | アップロードと登録の後、ローカルのファイルを消すまでの猶予（1h 以上） |
| `VMS_INGEST_STALE_AGE` | | `10m` | path ごとの最新のセグメントを、書き終わったとみなす mtime からの時間（2m 以上） |
| `VMS_RETENTION_INTERVAL` | | `10m` | 保持期間ジョブの間隔（1m〜24h）。`S3_*` を設定すると動く |
| `VMS_DISK_HIGH_WATERMARK` | | `90` | 録画ディレクトリのファイルシステムの使用率（%、`df` の Use%）がこれ以上でローカルのセグメントを消し始める（50〜99） |
| `VMS_DISK_LOW_WATERMARK` | | HIGH − 5 | これを下回るまで消す（1 以上、HIGH 未満） |
| `VMS_DISK_CHECK_INTERVAL` | | `30s` | 使用率を確かめる間隔（5s〜10m） |

不正な値や `CHANGE_ME` のままの値があると、すべての問題を表示して起動しません。

## 認証

設計は `docs/design.md` の「認証」。要点だけ書きます。

- セッション Cookie は `__Host-vms_session`（`Secure`）なので HTTPS が必要。開発環境も Caddy（`tls internal`）経由でアクセスする
- 状態を変えるリクエスト（POST / PATCH / DELETE など）には、公開オリジンと一致する `Origin` ヘッダーが必要（CSRF 対策）。curl で試すときは `-H "Origin: https://vms.localhost"` を付ける
- 接続元 IP（ログインの回数制限に使う）は、`VMS_TRUSTED_PROXIES` を設定しないと Caddy のアドレスになる（全員が同じ IP として数えられる）。開発環境で API をホストで動かす場合は、Caddy のコンテナから見える Docker のネットワーク（例: `192.168.0.0/16,172.16.0.0/12`）を設定する
- 操作を追加するときは、`api/openapi.yaml` で `x-min-role`（`admin` か `viewer`）を必ず書く。認証の要らない操作は `security: []`。どちらもないと API が起動しない。`internal/api/authz` のテストの表にも追加する

### 初期 admin と復旧

- `VMS_ADMIN_USERNAME` / `VMS_ADMIN_PASSWORD` を設定して起動すると、admin が 1 人もいないときだけ作る。既存のユーザーは変更しない
- CLI でも作れる。パスワードは標準入力から読む（端末なら 2 回入力、パイプなら 1 行目）

```sh
api create-admin <username>      # admin を作る
api reset-password <username>    # パスワードを変え、そのユーザーのセッションをすべて消す

# 例（compose の中で）
docker compose exec -it api api reset-password admin
```

## カメラと MediaMTX

設計は `docs/design.md` の「MediaMTX」と「カメラ管理」。

- カメラの一覧（`GET /api/cameras`）は viewer も使える。返すのは `id`、`name`、`enabled`、`has_sub` だけ。URL などの設定の取得、登録、変更、削除は admin だけ
- URL は `rtsp://` か `rtsps://`。URL に認証情報（`user:password@`）を入れると 400。認証情報は `credentials`（`username`、`password`）で渡す
  - 認証情報は書き込み専用。API は返さず（`has_credentials` だけ）、ログにも出さない。変更は `credentials` で置き換え、`remove_credentials: true` で消す
- 録画が残っているカメラは削除できない（409）。止めるだけなら `enabled: false` にする（MediaMTX から path を外す）
- 開発環境のテスト映像は `rtsp://testsrc:8554/main` と `rtsp://testsrc:8554/sub`（MediaMTX のコンテナから見た URL）

```sh
curl -sk -b cookies -H "Origin: https://vms.localhost" -H "Content-Type: application/json" \
  -d '{"name":"testsrc","main_url":"rtsp://testsrc:8554/main","sub_url":"rtsp://testsrc:8554/sub"}' \
  https://vms.localhost/api/cameras
```

### MediaMTX への反映

- API は、有効なカメラごとに `cam_<uuid>_main`（`record: true`、常時接続）と `cam_<uuid>_sub`（`record: false`、`sourceOnDemand: true`。sub の URL があるときだけ）を Control API で反映する。source は URL に認証情報を入れたもの
- 差分同期: `GET /v3/config/paths/list` と比べ、違うものだけ add / replace / delete する。`cam_` で始まらない path には触らない
- 同期するのは、起動時、カメラの変更時、`VMS_MEDIAMTX_SYNC_INTERVAL` ごと（MediaMTX を再起動すると Control API で足した path は消えるため）。失敗したら 1 秒から間隔までの指数バックオフでやり直す。MediaMTX が落ちていても API は起動する
- 認証情報を復号できないカメラ（`VMS_SECRET_KEY` を変えた、など）は、MediaMTX の path をそのままにしてエラーをログに出す
- Control API には `VMS_MEDIAMTX_API_USER` / `VMS_MEDIAMTX_API_PASSWORD` の Basic 認証で名乗る。MediaMTX は `authMethod: http` でこれを API の認証エンドポイント（#10）に問い合わせる。そのエンドポイントができるまでは、同期は 401 で失敗し続ける
## 録画の取り込み

設計は `docs/design.md` の「録画パイプライン」。

- MediaMTX がセグメントを書き終えると `POST /api/internal/mediamtx/segment-complete` を呼ぶ（`X-VMS-Hook-Token`）。API は検証して `pending_uploads` に積む。取りこぼしは起動時と定期のディレクトリ走査が拾う
- ワーカーが ffprobe で長さを測り、ffmpeg でサムネイルを作り、Garage に `<camera_id>/<YYYY>/<MM>/<DD>/<HH>/<started_at_unix_ms>.mp4`（と `.jpg`）でアップロードして `recordings` に登録する
- ローカルのファイルは、登録してから `VMS_INGEST_DELETE_GRACE`（1 時間以上）たってから消す。Garage が落ちている間はローカルに残してリトライし続ける
- 開発環境（API をホストで動かす）では、ホストに ffmpeg / ffprobe が要る。LGPL ビルドを使うなら Linux では `docker/ffmpeg` のイメージから `/opt/ffmpeg` を取り出す（`server.yml` の CI と同じ手順）
- ホストの API は `S3_ENDPOINT`（Caddy の `s3.<host>`）経由で Garage に接続する。Caddy のローカル CA を信頼させておく（`deploy/README.md`）

### ディスク逼迫時の処理

設計は `docs/design.md` の「ディスク逼迫時の処理」。取り込みが有効なときだけ動く。

- 使用率は `df` の Use% と同じ `(blocks - bfree) / (blocks - bfree + bavail)`（root 用の予約ブロックは分母に入れない）
- HIGH 以上になったら、LOW を下回るまで、(1) アップロード済みのローカルのコピー（猶予中でも。消す前に照合する）、(2) 未アップロードのセグメント、の順に古いものから消す。(2) は 1 ファイルごとに `events` の `disk_pressure`（`action: segment_discarded`）に残る
- path ごとの最新のファイル、`pending_uploads` に行のないファイル、ワーカーが処理中の行には触らない。失敗はログに出して次の確認でやり直す（録画は止めない）

## 保持期間

設計は `docs/design.md` の「保持期間」。`S3_*` を設定すると、起動時と `VMS_RETENTION_INTERVAL` ごとに動く。

- `ended_at` がカメラの `retention_days` より前の録画を 500 件ずつ、動画とサムネイルをまとめて DeleteObjects で消し、両方が消えた録画だけ `recordings` から消す
- 消せなかったものは残して次の回にやり直す（ログと `events` の `retention_delete_failed`）
- PostgreSQL の advisory lock で、複数のプロセスが同時に走らないようにする

### テスト

`internal/retention`、`internal/ingest` と `internal/s3store` の結合テストは、PostgreSQL と Garage（`dxflrs/garage:v2.5.0`）を testcontainers で起動する。
ffmpeg を使うテストは `VMS_TEST_FFMPEG_DIR`（`ffmpeg` と `ffprobe` があるディレクトリ）か PATH の ffmpeg を使い、どちらもなければ飛ばす。CI では LGPL ビルドを使い、`VMS_TEST_REQUIRE_FFMPEG=1` で飛ばさせない。
