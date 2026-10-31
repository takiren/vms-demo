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
  - 差分同期: Control API の path の一覧と比べ、違う `cam_` の path だけを add / replace / delete する。`cam_` で始まらない path には触らない
  - 起動時、カメラの変更時に加えて、定期的（既定 30 秒）にも同期する（MediaMTX を再起動すると Control API で足した path が消えるため）。MediaMTX に届かなくても API の起動は止めず、バックグラウンドでやり直す。同期は同時に 1 つだけ
  - 無効（`enabled: false`）のカメラは path を外す。認証情報を復号できないカメラは、path を変えずにエラーを記録する（録画を止めない側に倒す）
  - `_main`: `record: true`、常時接続。`_sub`: `record: false`、`sourceOnDemand: true`（見ている人がいるときだけカメラにつなぐ）。sub の URL がなければ作らない
  - source は DB の URL に、復号した認証情報を userinfo として入れたもの。source を含む設定や MediaMTX の応答はログに出さない
  - API は Control API に Basic 認証（`VMS_MEDIAMTX_API_USER` / `VMS_MEDIAMTX_API_PASSWORD`）で名乗る。MediaMTX はこれを `authMethod: http` で API に問い合わせ、API はこの組にだけ `action: api` を許可する
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
6. ディスク使用率が 90% を超えたら、アップロード済みのものから、それでも足りなければ未アップロードの古いものから捨てて `events` に記録する。録画は止めない

### 取り込みの詳細

- フック（`POST /api/internal/mediamtx/segment-complete`）は `X-VMS-Hook-Token` を `VMS_HOOK_TOKEN` と定数時間で比較する。トークンが未設定ならエンドポイントは 503（開かない）
- パスの検証（フックとディレクトリ走査の両方、削除の前にも）
  - MediaMTX から見たディレクトリ（`/recordings`）と API から見たディレクトリ（開発ではホストの `deploy/data/recordings`）は違う。相対パスで対応づける
  - `<録画ディレクトリ>/cam_<uuid>_main/<YYYY-MM-DD_HH-MM-SS-ffffff>.mp4` の形だけを受け付ける。`.` `..` を含むもの、シンボリックリンクを通るもの、通常のファイルでないものは拒否する。カメラが DB にないものも積まない
- ディレクトリ走査は起動時と定期（既定 5 分）。書き込み中のセグメントは拾わない
  - MediaMTX は path ごとに 1 つずつ書くので、書き込み中になりうるのは path ごとに一番新しいファイルだけ。それは mtime が `VMS_INGEST_STALE_AGE`（既定 10 分）より古いとき（録画が止まった）だけ拾う。ほかのファイルも mtime が 15 秒以上前のものに限る
- 開始時刻はファイル名から取る。MediaMTX はファイル名をプロセスのローカル時刻（コンテナの TZ。既定は UTC）で書くので、`VMS_RECORDINGS_TIMEZONE`（既定 UTC）で解釈して UTC で保存する。長さは ffprobe で映像のパケットの時刻から測る（fMP4 のヘッダーには全体の長さがない）。ffprobe が失敗したら mtime から推定する
- サムネイルは先頭のキーフレームを 320 px 幅の JPEG にする。失敗しても動画はアップロードし（`thumbnail_key` は NULL）、`thumbnail_failed` を記録する
- 順序は動画 → サムネイル → 同じトランザクションで `recordings` の upsert（`object_key` で一意。同じキーで開始時刻が違えば衝突としてリトライ扱い）と `pending_uploads.uploaded_at` の記録
- 完了した行はすぐには消さない。`uploaded_at` を入れ、`next_attempt_at` を猶予（`VMS_INGEST_DELETE_GRACE`、既定・最短 1 時間）の後にする。猶予が過ぎたらワーカーがローカルのファイルを消し、それから行を消す。行がある間は、走査が同じファイルを積み直さない
  - 消す前に確かめる: 今のファイルのサイズと mtime が、アップロードしたとき（`uploaded_size` / `uploaded_mtime`）と同じで、Garage の HEAD でオブジェクトが同じサイズで存在すること
  - ファイルが変わっていた（書き込み中の最新セグメントを上げてしまい、後から書き足された）とき、オブジェクトがない・サイズが違うときは、消さずに未アップロードに戻してアップロードし直す（同じキーを上書きし、`recordings` のサイズと終了時刻を更新する）。HEAD が失敗したときは消さずに後でリトライする
- 失敗したら `next_attempt_at` を指数バックオフ（5 秒から倍々、上限 5 分、ジッターあり）で先にして、いつまでもリトライする。`upload_failed` は 1 回目と 20 回ごとにだけ `events` に記録する
- リース（既定 15 分）を失ったワーカーは、行もファイルも変更しない。シャットダウンで中断したときは、リースを手放す（`attempts` は増やさない）
- ffmpeg / ffprobe には最小限の環境変数だけを渡す（API の環境には S3 の鍵やフックのトークンがある）。入力は `file:` を付けて渡す

### ディスク逼迫時の処理

- API が録画ディレクトリのファイルシステムの使用率を定期的に（`VMS_DISK_CHECK_INTERVAL`、既定 30 秒）確かめる。使用率は `df` の Use% と同じ計算（`(blocks - bfree) / (blocks - bfree + bavail)`。root 用の予約ブロックは使えない領域として分母から除く）
- 使用率が `VMS_DISK_HIGH_WATERMARK`（既定 90%）以上になったら、`VMS_DISK_LOW_WATERMARK`（既定 85%）を下回るまで、次の順にローカルのファイルを消す（ヒステリシス）。1 つ消すたびに使用率を測り直す
  1. アップロード済みの行（`uploaded_at` あり）を古い順に。猶予（`VMS_INGEST_DELETE_GRACE`）が過ぎていなくても消す。猶予は安全のための余裕で、ディスクが一杯のときは Garage にあるコピーから捨てるのが正しい。消す前の照合は猶予後の削除と同じ（サイズと mtime、Garage の HEAD）。HEAD が「ない」「サイズが違う」と答えたら、消さずにアップロードし直させる
     - HEAD そのものが失敗した（Garage に届かない、5xx、タイムアウト）ときは、アップロード時の記録で代える: ローカルのサイズと mtime が `uploaded_size` / `uploaded_mtime` と同じで、`recordings` に同じ `object_key` と `size_bytes` の行があれば消す。Garage が落ちている間に、アップロード済みのコピーを残して唯一のコピー（未アップロード）を捨てるのを避けるため。記録もなければ元の予定に戻す
     - この代わりの照合はディスク逼迫時だけ。猶予後の通常の削除は HEAD の成功を必須のままにする
     - 消したコピーは 1 件ずつ `events` に `disk_pressure`（`action: uploaded_copy_removed`、`verified: head` か `upload_record`）を記録する
  2. それでも足りなければ、未アップロードの行を古い順に捨てる。1 ファイルごとに `events` に `disk_pressure`（`action: segment_discarded`、camera_id、パス、サイズ、開始時刻）を記録する。捨てるのは逼迫したときだけで、1 ファイルが 1 分の録画に当たるので、件数はディスクの容量で抑えられる
- 対象は `pending_uploads` の行だけ。先にディレクトリ走査で、まだ積まれていないファイルを積む。行のないファイル（カメラが DB にない path など）には触らない
- path ごとに一番新しいファイル（書き込み中かもしれない）は、どちらの段階でも消さない。パスの検証は取り込みと同じ（録画ディレクトリの外やシンボリックリンクには触らない）
- ワーカーと同じリースで行を取る。ワーカーが処理中の行（`leased_until` が未来）は飛ばす。猶予中やバックオフ中で待っている行は取れる。消さなかった行は、取る前の `next_attempt_at` に戻す
- 録画は止めない。失敗はログに出して、次の確認でやり直す。何も消せずに閾値を超えたままのときは、`disk_pressure`（`action: unresolved`）を 10 分に 1 回だけ記録する

### 保持期間

- 保持期間はカメラごとの `retention_days`（デフォルト 7 日）
- 10 分ごと（`VMS_RETENTION_INTERVAL`）のジョブが、期限切れのものを S3 の DeleteObjects で消す。削除に成功したものだけ DB からも消す
  - 期限切れは `ended_at` が `retention_days` より前のもの（保持期間に少しでもかかる録画は消さない）
  - 500 件ずつ古い順に（`started_at`, `id` のキーセットで）処理する。動画とサムネイルを同じ DeleteObjects で消し、両方（サムネイルがあれば）が削除に成功した録画だけ DB から消す。存在しないキーも削除の成功として返る（S3 の仕様、Garage でも同じ）ので、前回の途中で失敗したものは次の実行で片づく
  - 失敗したものは DB に残し、ログと `events`（`retention_delete_failed`、1 回の実行で 1 件にまとめる）に記録して次の実行でやり直す
  - 複数のプロセスが同時に走らないよう、PostgreSQL の advisory lock（取れなければその回は何もしない）を取る
- Garage の lifecycle には頼らない

## ストレージ（Garage）

- 単一ノード、`replication_factor=1`
- バケットは `recordings` の 1 つだけ。CORS を設定する
- キーの形式
  - 動画: `<camera_id>/<YYYY>/<MM>/<DD>/<HH>/<started_at_unix_ms>.mp4`
  - サムネイル: 動画と同じキーで拡張子が `.jpg`
- layout、キー、バケット、CORS は init コンテナで冪等に設定する。キーは `.env` で固定する
- API は 2 つのエンドポイントを使い分ける（クライアントは aws-sdk-go-v2、パス形式）
  - `S3_ENDPOINT`（`https://s3.<host>`）: presigned URL の署名だけに使う。ブラウザが使うホストで署名しないと壊れる
  - `S3_INTERNAL_ENDPOINT`: API 自身のアップロードと削除。全部コンテナの構成では `http://garage:3900`。開発（API はホスト）では省略して `S3_ENDPOINT`（Caddy 経由）を使う。Garage の 3900 はホストに公開しない

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
| `pending_uploads` | local_path, attempts, uploaded_at（アップロードと登録が済んだ時刻。猶予が過ぎるまで行を残す）, uploaded_size, uploaded_mtime（消す前の照合用）, leased_until（処理中のリースの期限。処理中でなければ NULL） |
| `events` | ディスク逼迫、アップロード失敗、カメラの切断など |

- RTSP の認証情報は AES-256-GCM で暗号化する。鍵は環境変数 `VMS_SECRET_KEY` で渡す
  - 形式: `バージョン（1 バイト、今は 1）|| nonce（12 バイト、毎回 crypto/rand）|| 暗号文 || タグ`。中身は `{"username","password"}` の JSON
  - AAD にバージョンとカメラの ID を入れる。暗号文を別の行にコピーしても復号できない。そのため ID（UUIDv7）は API が決めてから挿入する
  - バージョンの 1 バイトは、鍵のローテーション（Phase 2）で新旧を区別するためのもの
- `username` は ASCII（`[A-Za-z0-9._-]`、64 文字まで）に限り、大文字小文字を区別せずに一意にする（`lower(username)` の一意インデックス）
- `sessions` にはセッショントークンの SHA-256 だけを保存する。生のトークンは保存しない
- `cameras.main_url` / `sub_url` に認証情報（userinfo、`user:pass@`）を入れない。CHECK で拒否する
- `pending_uploads` は `next_attempt_at` をリースにする。完了（削除）と失敗の記録は、取得時に返った `next_attempt_at` が一致するときだけ成功する（リースを失ったワーカーは書き換えられない）
  - 取得したときは `leased_until` にもリースの期限を入れ、処理を終えたら（失敗、完了、戻し）NULL にする。待っている行と処理中の行を区別するため（ディスク逼迫時の処理が使う）
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

### カメラ管理

- 一覧（`GET /api/cameras`）は viewer も使える。返すのは ID、名前、有効か、sub があるかだけ（URL はカメラの内部アドレスなので返さない）。設定の取得、登録、変更、削除は admin だけ
- URL は `rtsp://` / `rtsps://` だけ。userinfo を含む URL は 400（DB の CHECK でも拒否する）
- 認証情報は書き込み専用。応答には `has_credentials` だけを入れ、ログにも出さない。変更では、置き換えるか（`credentials`）、消すか（`remove_credentials: true`）を明示する。どちらもなければそのまま
- 録画が残っているカメラは削除できない（409）。録画を止めるだけなら無効にする

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
