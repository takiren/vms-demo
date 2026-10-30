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
- `internal/testutil/pgtest`: 結合テスト用に PostgreSQL のコンテナとテストごとの DB を用意する
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
