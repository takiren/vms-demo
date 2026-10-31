# server

Go による API サーバーの実装ディレクトリです。

- `cmd/api`: API サーバーのエントリポイント
- `internal/config`: 環境変数からの設定の読み込みと検証
- `internal/api`: ルーティングとハンドラ
  - `gen/`: `api/openapi.yaml` から oapi-codegen で生成したコード（手で編集しない）
  - `apierror/`: 共通エラー形式への変換
  - `validation/`: OpenAPI によるリクエスト検証のミドルウェア
- `internal/`: そのほかのビジネスロジック、カメラ管理、録画パイプライン、DB アクセスなど

## 開発

Go 1.27 が必要です。ツール（oapi-codegen、air、golangci-lint）はバージョンを固定して `go tool` / `go run` で動かすので、別途インストールは要りません。

```sh
cd server
make generate   # api/openapi.yaml からコードを生成する
make build      # go build ./...
make test       # go test -race（カバレッジは coverage.out）
make lint       # golangci-lint
make dev        # air でホットリロードしながら起動する（../.env を読む）
```

`api/openapi.yaml` を変えたら `make generate` して、生成物もコミットしてください。CI で差分を確認しています。

## 設定（環境変数）

| 変数 | 必須 | 既定値 | 説明 |
|---|---|---|---|
| `DATABASE_URL` | ○ | | PostgreSQL の接続 URL（`postgres://` か `postgresql://`） |
| `VMS_SECRET_KEY` | ○ | | 32 バイトの鍵を base64 にしたもの（`openssl rand -base64 32`） |
| `VMS_LISTEN_ADDR` | | `:8080` | 待ち受けアドレス |
| `VMS_LOG_LEVEL` | | `info` | `debug` / `info` / `warn` / `error` |
| `VMS_SHUTDOWN_TIMEOUT` | | `10s` | SIGINT / SIGTERM の後、処理中のリクエストを待つ時間 |

不正な値や `CHANGE_ME` のままの値があると、すべての問題を表示して起動しません。
