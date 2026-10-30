# deploy

開発用の docker compose と、各コンポーネントの設定です。

| ファイル | 内容 |
|---|---|
| `compose.yaml` | 開発用のインフラ（リポジトリ直下の `compose.yaml` から include する） |
| `Caddyfile` | リバースプロキシ（単一オリジン、`s3.<host>` は Garage） |
| `garage.toml` | Garage（単一ノード、`replication_factor = 1`）。秘密情報は環境変数 |
| `garage-init/init.py` | Garage の layout、キー、バケット、CORS を冪等に設定する（`garage-init` コンテナ） |
| `mediamtx.yml` | MediaMTX の全体設定（[docker/mediamtx/README.md](../docker/mediamtx/README.md)） |
| `init-env.sh` | `.env.example` から、秘密情報をランダムに埋めた `.env` を作る |
| `smoke.sh`、`smoke/` | compose の結合テスト（CI の `deploy.yml` でも実行する） |
| `data/` | 録画の一時置き場（`data/recordings`）。`.gitignore` で除外 |

## 開発環境の起動

compose で動かすのはインフラだけです。API（air）と web（vite dev）はホストで動かし、Caddy から `host.docker.internal` で届きます。

```sh
deploy/init-env.sh        # .env を作る（cp .env.example .env して CHANGE_ME を全部書き換えてもよい）
docker compose up         # リポジトリ直下で。初回は ffmpeg のビルドに数分かかる
```

- リポジトリ直下の `compose.yaml` が `deploy/compose.yaml` を include し、直下の `.env` で変数を展開します（`deploy/` で `docker compose` を実行しないこと。`.env` が読まれない）
- Linux では `.env` の `MEDIAMTX_UID` / `MEDIAMTX_GID` を自分の `id -u` / `id -g` にします（下記）
- 止めるとき: `docker compose down`。データ（Postgres、Garage、Caddy の CA）も消すとき: `docker compose down -v` と `rm -rf deploy/data`

### URL

| URL | 中身 |
|---|---|
| `https://vms.localhost/` | web（ホストの vite dev、`:5173`） |
| `https://vms.localhost/api/...` | API（ホストの `:8080`）。`/api` は外さずに渡す |
| `https://vms.localhost/api/internal/...` | Caddy が 404 を返す（MediaMTX → API 専用。MediaMTX は Caddy を通さず API を呼ぶ） |
| `https://vms.localhost/live/hls/<path>/index.m3u8` | MediaMTX の LL-HLS（`:8888`） |
| `https://vms.localhost/live/webrtc/<path>/whep` | MediaMTX の WebRTC（`:8889`）。`reader.js` も同じ下にある |
| `https://s3.vms.localhost/recordings/...` | Garage の S3 API（presigned URL で取得する） |

ホストに公開するポート:

| ポート | 用途 |
|---|---|
| `80`、`443` | Caddy（`CADDY_HTTP_PORT` / `CADDY_HTTPS_PORT` で変えられる） |
| `127.0.0.1:5432` | Postgres（ホストの API から。`DATABASE_URL`） |
| `127.0.0.1:9997` | MediaMTX の Control API（ホストの API から） |
| `127.0.0.1:8189/udp,tcp` | WebRTC のメディア（LAN から見るときは `MTX_WEBRTC_BIND=0.0.0.0` と `MTX_WEBRTCADDITIONALHOSTS=<LAN の IP>`） |
| `127.0.0.1:8554` | テスト映像の RTSP（VLC などで見る用） |

Garage の S3（3900）、RPC（3901）、Admin API（3903）、MediaMTX の HLS / WebRTC（HTTP）は公開しません。

### Caddy のローカル CA を信頼する

TLS は Caddy の内部 CA（`tls internal`）です。ブラウザ（と、ホストの API が `S3_ENDPOINT` に HTTPS で接続するとき）に、この CA を信頼させます。

```sh
docker compose cp caddy:/data/caddy/pki/authorities/local/root.crt ./caddy-root.crt
# macOS
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain caddy-root.crt
# Ubuntu
sudo cp caddy-root.crt /usr/local/share/ca-certificates/caddy-local.crt && sudo update-ca-certificates
```

CA は `caddy-data` ボリュームにあるので、`docker compose down -v` すると作り直されます（もう一度信頼させる）。

`*.localhost` は多くの環境で 127.0.0.1 に解決されますが、解決されないときは `/etc/hosts` に `127.0.0.1 vms.localhost s3.vms.localhost` を足してください。

### 録画ディレクトリの所有者

MediaMTX は `deploy/data/recordings` を `/recordings` として bind mount して書きます。ホストの API は同じディレクトリを読み、アップロード後に消します。

- `recordings-init` が起動のたびにディレクトリを作り、`MEDIAMTX_UID:MEDIAMTX_GID` のものにします（モード 0750）。MediaMTX もこのユーザーで動きます
- Linux: `MEDIAMTX_UID` / `MEDIAMTX_GID` を自分の uid / gid にすると、ファイルが自分のものになり、ホストの API が消せます
- macOS（OrbStack / Docker Desktop）: bind mount のファイルはホストではいつも自分のものに見えるので、既定（10001）のままでよい

### Garage

`garage-init` が、起動のたびに次を冪等に設定して終わります（何度実行してもよい: `docker compose run --rm garage-init`）。

1. ノードに役割がなければ layout を割り当てて適用する（zone `dc1`、容量 `GARAGE_CAPACITY`）
2. `.env` の `S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY` を取り込む。同じ ID で別のシークレットが登録済みなら失敗する（上書きしない）
3. バケット `S3_BUCKET`（`recordings`）を作り、キーに read / write を許可する
4. CORS を設定する: Origin は `https://<VMS_HOST>` だけ、メソッドは GET / HEAD、ヘッダーは `Range` など。ワイルドカードは使わない

- キーの形式: ID は `GK` + 16 進数 24 文字、シークレットは 16 進数 64 文字（Garage が自分で作るキーと同じ形式。`init.py` が検査し、`CHANGE_ME` のままなら分かるメッセージで失敗する）
- Garage は、エラー応答（403 など）には CORS の設定にかかわらず `Access-Control-Allow-Origin: *` を付ける（v2.5 の挙動）。別オリジンの preflight は 403 になるのでブラウザは本体を送らず、成功した応答には自オリジン以外の許可は付かない。エラーの本文（コードとパス）が別オリジンから読めるだけで、オブジェクトは読めない
- CORS の設定（S3 の PutBucketCors）にはバケットの owner 権限が要るので、設定の間だけキーに owner を与え、最後に外す。API に残るのは read / write だけ
- Admin API と S3 の呼び出しは Python の標準ライブラリだけで行う（aws-cli などは入れない）
- 保持期間による削除は API のジョブが行う。Garage の lifecycle は使わない

### テスト映像

`testsrc` が `rtsp://testsrc:8554/main`（720p）と `/sub`（360p）を配信します。
API ができるまでの確認方法（Control API で path を手で足す）は [docker/testsrc/README.md](../docker/testsrc/README.md)。

## 結合テスト

```sh
deploy/smoke.sh     # 終わったら docker compose down -v する（SMOKE_KEEP=1 で残す）
```

ホストの :8080 で API のスタブ（`smoke/stub-api.py`）を動かし、次を確かめます。
Postgres（`uuidv7()`）、garage-init の 2 回目の実行、presigned URL の PUT / GET（Caddy の `s3.<host>` 経由）、CORS（自オリジンだけ）、
`/api` と `/api/internal` のブロック、公開していないポート、テスト映像の取り込み（Control API の `ready`）、`/live/hls` と `/live/webrtc`、録画の書き込みとホストからの削除、セグメント完了の通知。
