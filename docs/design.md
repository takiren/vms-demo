# 設計

## 前提

- OSS として公開する。自分のコードは Apache-2.0
- サーバー 1 台構成（Ubuntu）。複数台構成はスコープ外
- カメラは最大 32 台程度。H.264 / H.265 が混在
- MVP のスコープ
  - カメラは RTSP の URL を手動で登録する
  - 録画は常時録画のみ
  - トランスコードはしない

## 全体構成

```
Browser ──> Caddy ─┬─ /         web (静的ファイル)
                   ├─ /api      Go API ──> PostgreSQL
                   ├─ /live/    MediaMTX (WebRTC / LL-HLS)
                   └─ s3.<host> Garage (presigned URL で直接取得)

Camera ──RTSP──> MediaMTX ──fMP4──> 共有ボリューム ──> Go API ──> Garage
```

| コンポーネント | 役割 |
|---|---|
| MediaMTX | RTSP の取り込み、録画、WebRTC / LL-HLS 配信 |
| Go API | カメラ管理、認証、セグメントの取り込み、録画再生、保持期間による削除 |
| Garage | 録画とサムネイルの保存先（S3 互換） |
| PostgreSQL | メタデータ、録画の目録 |
| Caddy | 単一オリジンへのリバースプロキシ、TLS |
| ffmpeg | サムネイルの生成、録画の切り出し（API から CLI として exec で呼ぶ） |

## MediaMTX

- カメラ 1 台につき 2 つの path
  - `cam_<uuid>_main`: 録画用。単画面のライブにも使う
  - `cam_<uuid>_sub`: 多分割ライブ用（H.264）
- カメラの定義は DB が正。API が起動時と変更時に Control API で反映する
- `mediamtx.yml` には全体設定だけを書く。path の定義は書かない
- 録画: `recordFormat: fmp4`、`recordSegmentDuration: 1m`。main だけを録る
- 認証: `authMethod: http` で Go API に問い合わせる
- セグメントが完了したら `runOnRecordSegmentComplete` で API に通知する
  - 公式イメージには curl が入っていないので、curl 入りのイメージを自前でビルドする

## 録画パイプライン

1. MediaMTX がローカルの共有ボリュームにセグメントを書く
2. フックの通知を受けて `pending_uploads` に積む。取りこぼしは、起動時と定期実行のディレクトリ走査で拾う
3. 次の順に処理する
   1. ffmpeg で先頭のキーフレームを JPEG にする
   2. Garage にアップロードする
   3. `recordings` に登録する
4. アップロードと DB 登録が成功したら、ローカルのファイルを消す。猶予は最短 1 時間
5. Garage に障害があるときは、ローカルに溜めてリトライし続ける
6. ディスク使用率が 90% を超えたら、未アップロードの古いものから捨てて `events` に記録する。録画は止めない

### 保持期間

- 保持期間はカメラごとの `retention_days`（デフォルト 7 日）
- 10 分ごとのジョブが、期限切れのものを S3 の DeleteObjects で消す。削除に成功したものだけ DB からも消す
- Garage の lifecycle には頼らない

## ストレージ（Garage）

- 単一ノード、`replication_factor=1`
- バケットは `recordings` の 1 つだけ。CORS を設定する
- キーの形式
  - 動画: `<camera_id>/<YYYY>/<MM>/<DD>/<HH>/<started_at_unix_ms>.mp4`
  - サムネイル: 動画と同じキーで拡張子が `.jpg`
- layout、キー、バケット、CORS は init コンテナで冪等に設定する。キーは `.env` で固定する

## 再生

- ライブ
  - WebRTC（WHEP）がメイン。フォールバックは LL-HLS
  - クライアントは MediaMTX 同梱の `reader.js` を使う
  - 多分割表示は sub、単画面は main
- 録画
  - API が DB の目録から HLS の VOD プレイリストを生成する（fMP4、セグメントごとに `EXT-X-MAP` と `EXT-X-DISCONTINUITY`）
  - セグメントは presigned URL でブラウザが直接取得する
  - 再生は hls.js
- 切り出し: 指定した範囲のセグメントを ffmpeg で結合し（コピーのみ）、mp4 として返す

## DB（PostgreSQL）

- PostgreSQL 18 以上（イメージは `postgres:18.x-alpine`）
- ID はすべて UUIDv7。PostgreSQL 18 の `uuidv7()` を列の既定値にする
- 時刻はすべて `timestamptz`
- マイグレーションは goose の SQL ファイルをバイナリに埋め込み、API の起動時に advisory lock を取って適用する。失敗したら起動しない

| テーブル | 主なカラム |
|---|---|
| `users` | username, password_hash (argon2id), role (`admin` / `viewer`) |
| `sessions` | セッション Cookie に対応する |
| `cameras` | name, main_url, sub_url, credentials_enc, retention_days, enabled |
| `recordings` | camera_id, started_at, ended_at, object_key, size_bytes, thumbnail_key。インデックスは `(camera_id, started_at)` |
| `pending_uploads` | local_path, attempts |
| `events` | ディスク逼迫、アップロード失敗、カメラの切断など |

- RTSP の認証情報は AES-256-GCM で暗号化する。鍵は環境変数 `VMS_SECRET_KEY` で渡す
- `username` は ASCII（`[A-Za-z0-9._-]`、64 文字まで）に限り、大文字小文字を区別せずに一意にする（`lower(username)` の一意インデックス）
- `sessions` にはセッショントークンの SHA-256 だけを保存する。生のトークンは保存しない
- `cameras.main_url` / `sub_url` に認証情報（userinfo、`user:pass@`）を入れない。CHECK で拒否する
- `pending_uploads` は `next_attempt_at` をリースにする。完了（削除）と失敗の記録は、取得時に返った `next_attempt_at` が一致するときだけ成功する（リースを失ったワーカーは書き換えられない）
- 外部キーの `ON DELETE`
  - `sessions.user_id`: CASCADE（ユーザーを消したらセッションも消す）
  - `recordings.camera_id`: RESTRICT（録画が残っているカメラは消せない。先に S3 のオブジェクトと目録を消す。目録だけが消えて Garage にオブジェクトが残るのを防ぐ）
  - `events.camera_id`: SET NULL（イベントは記録として残す）

## 認証

- ログインはセッション Cookie（HttpOnly）
- ロールは `admin` と `viewer`
- ライブの閲覧は、MediaMTX からの問い合わせに API が判定する

### セッション

- トークンは `crypto/rand` の 32 バイトを base64url（パディングなし）にしたもの。DB には SHA-256 だけを保存する
- Cookie は `__Host-vms_session`。`HttpOnly`、`Secure`、`SameSite=Strict`、`Path=/`、`Domain` なし。有効期限は付けない（ブラウザのセッション Cookie）。期限はサーバー側で管理する
  - `Secure` なので HTTPS が前提。開発環境も Caddy の `tls internal` で HTTPS にする
- 期限は 2 つ。どちらかを過ぎたら無効
  - アイドル: 最後のアクセスから `VMS_SESSION_IDLE_TIMEOUT`（既定 12h）。アクセスのたびに延ばす（スライディング）
  - 絶対: 作成から `VMS_SESSION_MAX_LIFETIME`（既定 168h = 7 日）。延長しても超えない
- 延長（`last_seen_at` と `expires_at` の更新）は 1 分に 1 回まで（アイドル期限が短いときは、その 1/4 まで）。リクエストごとには書き込まない。期限の計算は DB の時計（`now()`）で行う
- 期限切れのセッションは API の中のジョブが 10 分ごとに消す
- ログインのたびに新しいトークンを発行する（セッション固定の防止）。ログイン時に Cookie で送られてきた古いセッションは消す
- パスワードの変更、ロールの変更、ユーザーの削除では、そのユーザーのセッションをすべて消す
  - 自分でパスワードを変えたときは、ほかのセッションを消したうえで、新しいセッションを発行する
  - ログインの検証からセッション作成までの間にパスワードが変わった場合は、セッションを作らない（作成時にユーザーの行をロックし、ハッシュが変わっていないことを確かめる）
- ログアウトは認証なしで受け付け、送られてきたセッションを消して Cookie を消す（`Max-Age=0`）

### パスワード

- argon2id（`golang.org/x/crypto/argon2`）。RFC 9106 の 2 番目の推奨値: m=64 MiB、t=3、p=4、ソルト 16 バイト、出力 32 バイト
  - PHC 形式（`$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>`）で保存する。パラメーターがハッシュに入っているので、後から変えられる。ログインに成功したとき、パラメーターが現在の設定と違えば作り直す
  - 比較は定数時間（`crypto/subtle`）
  - メモリを使い切らないよう、同時に計算するのは 2 つまで。5 秒待っても空かなければ 503 を返す
- ポリシー: Unicode のコードポイントで 12 文字以上 128 文字以下。文字種の条件は付けない。正規化はしない（入力されたバイト列をそのまま使う）
- 存在しないユーザー名でのログインも、存在するユーザーと同じエラー（401）を返し、ダミーのハッシュを検証して応答時間を揃える

### 総当たり対策

- ログインの失敗を、メモリ上でユーザー名ごと（15 分に 10 回）と接続元 IP ごと（15 分に 30 回）に数える。超えたら窓が明けるまで 429（`Retry-After` 付き）。成功したらユーザー名の分を戻す
  - サーバー 1 台構成なのでメモリで足りる。再起動すると数え直しになる
  - 記録するキーの数には上限（10 万）がある。上限に達したら、新しいキーは拒否する（安全側に倒す）
  - ユーザー名ごとの制限は、第三者がそのユーザーを一時的にロックアウトできてしまう（総当たりを防ぐための割り切り）。解除は、窓が明けるのを待つか API を再起動する
- 自分のパスワード変更で現在のパスワードを間違えた場合も、ユーザー名ごとの失敗として数える
- 接続元 IP は、直接の接続元が `VMS_TRUSTED_PROXIES`（CIDR のリスト）に入っているときだけ `X-Forwarded-For` を右から辿って求める。既定は空（ヘッダーを信用しない）。Caddy の後ろで動かすときは Caddy のアドレスを設定する

### CSRF

- `SameSite=Strict` に加えて、状態を変えるメソッド（GET / HEAD / OPTIONS 以外）では、`Origin` が公開オリジン（`https://<VMS_HOST>`、`VMS_PUBLIC_ORIGIN` で上書きできる）と一致することを求める。`Origin` がなければ `Sec-Fetch-Site: same-origin` を求める。どちらもなければ 403
  - ログインにも適用する（ログイン CSRF の防止）。curl などで叩くときは `Origin` ヘッダーを付ける
  - `/api/internal/` 以下（MediaMTX からの呼び出し。後の issue）は対象外にして、別の方法で認証する

### 認可

- 既定で拒否する。OpenAPI の `security` を操作ごとに強制する
  - 全体の既定は `security: [cookieAuth: []]`。認証の要らない操作（`healthz`、ログイン、ログアウト）だけ `security: []` で外す
  - 認証が要る操作には、必要な最低ロールを `x-min-role`（`admin` か `viewer`）で必ず書く。`viewer` はログインしていれば誰でもよい
  - 認証の要らない操作に `x-min-role` を書いたもの、認証が要る操作で `x-min-role` がないもの、`cookieAuth` 以外の方式のものがあれば、API は起動しない
- 判定はミドルウェアで行い、ハンドラでは行わない
  - Echo のルート（メソッドとパスのパターン）から操作を引き、リクエストの検証より前に判定する（未認証は 401、ロール不足は 403）
  - 念のため strict-server のミドルウェアでも、操作 ID で同じ判定をする。表にない操作は拒否する
- ログイン中のユーザーは `context.Context` に入れる（`auth.UserFrom(ctx)`）
- ロールはリクエストのたびに DB から読む（セッションとユーザーを結合して取得する）

### ユーザー管理

- admin だけがユーザーの一覧、作成、取得、更新（ロール、パスワード）、削除をできる。応答に `password_hash` は含めない
- 最後の admin は削除も降格もできない（409）。admin の行をすべて `FOR UPDATE` でロックしてから数えるので、同時に実行しても admin が 0 人にならない
- 自分自身は削除できない（409）。自分のパスワードは `POST /api/auth/password`（現在のパスワードが必要）で変える
- 初期 admin
  - 環境変数 `VMS_ADMIN_USERNAME` / `VMS_ADMIN_PASSWORD`: 起動時に admin が 1 人もいないときだけ作る。既存のユーザーは変更しない（同じ名前のユーザーがいれば警告して何もしない）。パスワードはログに出さない。`CHANGE_ME` やポリシーを満たさないパスワードなら起動しない
  - CLI: `api create-admin <username>`、`api reset-password <username>`。パスワードは標準入力から読む（引数には取らない）。`reset-password` はそのユーザーのセッションをすべて消す

## API サーバー

- 使うもの
  - Go 1.27
  - Echo v5
  - pgx、sqlc、goose
  - oapi-codegen v2（`echo5-server` と `strict-server`）。OpenAPI 定義によるリクエスト検証のミドルウェアも使う
- OpenAPI ファースト（`api/openapi.yaml`）。フロントの型は openapi-typescript で生成する
- strict-server で対応しにくいものの扱い
  - `Set-Cookie` は OpenAPI にヘッダーとして定義する
  - ログイン中のユーザーは、ミドルウェアで `context.Context` に入れる
  - ストリームで返すものは `io.Reader` で返す
- エラーはすべて `{"error": {"code": "...", "message": "..."}}` で返す（`api/openapi.yaml` の `Error`）
  - 5xx では内部の詳細を返さず、ログにだけ出す
- 設定は環境変数から読む。起動時にまとめて検証し、不正なら起動しない。秘密情報はログに出さない

## フロントエンド

- 使うもの: Vite、React、TanStack Router、TanStack Query、Tailwind CSS、hls.js
- 画面
  1. ログイン
  2. 多分割ライブ（1 / 4 / 9 / 16）
  3. カメラ単画面（ライブ ⇔ 録画再生の切り替え、24 時間のタイムラインとサムネイル）
  4. 切り出しとダウンロード
  5. カメラ管理（admin）
  6. ユーザー管理（admin）

## ffmpeg

- `--disable-gpl --disable-nonfree --enable-shared` でビルドする
- 含めるもの: ネイティブの h264 / hevc デコーダー、mjpeg エンコーダー、mp4 muxer など最小限
- Ubuntu 24.04 ベースの multi-stage Dockerfile でビルドし、共有ライブラリごとイメージに同梱する
- LGPL への対応として、ソースとビルド手順を公開する

## リポジトリ構成

```
server/   Go (cmd/api, internal/...)
web/      フロントエンド
api/      openapi.yaml
deploy/   compose.yaml, mediamtx.yml, garage.toml, Caddyfile
docker/   ffmpeg / mediamtx の Dockerfile
docs/     設計ドキュメント
```

## 開発・運用

- 開発時の起動
  - compose で起動するもの: Caddy、Postgres、Garage（と init コンテナ）、MediaMTX、テスト映像の配信
  - ホストで動かすもの: API（air）、web（vite dev）。Caddy から `host.docker.internal` で到達する
  - リポジトリ直下で `docker compose up`。直下の `compose.yaml` が `deploy/compose.yaml` を include し、直下の `.env` で展開する
  - テスト映像は GStreamer（OpenH264）で作る。開発用でも GPL のコンポーネントは使わない
- 全部コンテナで動かす構成は `compose.prod.yaml` にする
- バージョンはマイナーまで固定し、更新は Renovate に任せる
- テスト: Go のユニットテストと testcontainers での結合テスト、web は Vitest
- CI: GitHub Actions で lint、test、ffmpeg イメージのビルド

## Phase 2

- ONVIF（自動検出、PTZ、イベント）
- イベント録画
- カメラ単位の閲覧権限
- OIDC
- 暗号鍵のローテーション
- システム状態の画面
- E2E（Playwright）
- 複数台構成
