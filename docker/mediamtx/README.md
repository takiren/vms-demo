# MediaMTX イメージ

MediaMTX（v1.21.3、MIT）に curl、jq、LGPL ffmpeg を足したイメージです。
公式イメージには curl がなく、`runOnRecordSegmentComplete` から API を呼べないので自前でビルドします。

| ファイル | 内容 |
|---|---|
| `Dockerfile` | Ubuntu 24.04 ベース。`mediamtx` は公式イメージから、ffmpeg は `docker/ffmpeg` のイメージから取り込む |
| `mtx-segment-complete.sh` | セグメント完了を API に通知するスクリプト（イメージの `/usr/local/bin/mtx-segment-complete`） |
| `LICENSE-mediamtx` | MediaMTX の MIT ライセンス（イメージの `/usr/share/doc/mediamtx/LICENSE`） |

設定ファイルはイメージに入れません。`deploy/mediamtx.yml` を `/etc/mediamtx/mediamtx.yml` にマウントします。

## ビルド

ffmpeg のイメージを先に作り、`FFMPEG_IMAGE` で指定します（既定は `vms-ffmpeg:local`）。

```sh
docker build -t vms-ffmpeg:local docker/ffmpeg
docker build -t vms-mediamtx docker/mediamtx
# イメージ名が違うとき
docker build --build-arg FFMPEG_IMAGE=vms-ffmpeg:ci docker/mediamtx
```

compose では、`build.args` で `FFMPEG_IMAGE` を渡すか、named build context で渡す（Dockerfile は `FROM ${FFMPEG_IMAGE} AS ffmpeg`）。

```yaml
build:
  context: ../docker/mediamtx
  args:
    FFMPEG_IMAGE: vms-ffmpeg:local
```

- 非 root（uid/gid 10001）で動く。`/recordings` はこのユーザーのもの。名前付きボリュームなら、初回に所有者を引き継ぐ。bind mount のときは `chown 10001:10001` が要る
- ffmpeg は `/opt/ffmpeg`（PATH 済み）。ライセンスとソースも同梱される
- glibc 2.39 以上が必要なので、ベースを Alpine に変えない

## 実行

```sh
docker run --rm \
  -v "$PWD/deploy/mediamtx.yml:/etc/mediamtx/mediamtx.yml:ro" \
  -v recordings:/recordings \
  -e VMS_HOOK_TOKEN=... \
  vms-mediamtx
```

### compose（#3）が知っておくこと

| 項目 | 内容 |
|---|---|
| ボリューム | `recordings` を MediaMTX の `/recordings`（rw）と API の同じパス（読み取りと削除）で共有する |
| ポート（公開しない） | `9997` Control API、`8888` HLS、`8889` WebRTC（HTTP）。Caddy と API からだけ到達できるようにする |
| ポート（公開する） | `8189/udp` と `8189/tcp`（WebRTC のメディア）。Caddy は通さない |
| 環境変数 | `VMS_HOOK_TOKEN`（必須。空だと通知しない）。`MTX_AUTHHTTPADDRESS`、`VMS_HOOK_URL`、`MTX_WEBRTCADDITIONALHOSTS` |
| host.docker.internal | API をホストで動かす開発構成では `extra_hosts: ["host.docker.internal:host-gateway"]` が要る |
| 開発でホストの API が Control API を呼ぶとき | `127.0.0.1:9997:9997` にだけ公開する |

## 環境変数で上書きできる設定

MediaMTX は、設定のキーを `MTX_` + 大文字にした環境変数で上書きできる（path の既定値は `MTX_PATHDEFAULTS_...`）。

| 変数 | 既定（yml） | 用途 |
|---|---|---|
| `MTX_AUTHHTTPADDRESS` | `http://host.docker.internal:8080/api/internal/mediamtx/auth` | 認証の問い合わせ先（全部コンテナなら `http://api:8080/...`） |
| `MTX_WEBRTCADDITIONALHOSTS` | なし | ブラウザに伝える ICE candidate のホスト（公開 IP / ホスト名） |
| `MTX_LOGLEVEL` | `info` | ログレベル |
| `VMS_HOOK_URL` | `http://host.docker.internal:8080/api/internal/mediamtx/segment-complete` | セグメント完了の通知先（スクリプトが読む） |
| `VMS_HOOK_TOKEN` | なし（必須） | 通知の共有シークレット。`openssl rand -hex 32` で生成して `.env` に書く |

## 設定の方針

- **path は書かない。** `paths: {}` にして `all_others` も置かない。API が Control API で追加していない path への接続は拒否される。API は起動時に必ず再同期する（MediaMTX の再起動で Control API 経由の path は消える）
- **録画は path 単位。** `pathDefaults.record: false` で、API が `_main` の path だけ `record: true` にする。形式は fMP4、セグメントは 1 分、保存先は `/recordings/%path/%Y-%m-%d_%H-%M-%S-%f`。`recordDeleteAfter: 0s` で、削除は API のジョブだけが行う
- **使わないものは無効。** RTSP サーバー、RTMP、SRT、MoQ、metrics、pprof、playback。カメラへは MediaMTX が RTSP クライアントとして接続する（サーバーは要らない）
- **CORS は許可しない。** Caddy で単一オリジンにするため。HLS / WebRTC の `allowOrigins` は空

### 認証（authMethod: http）

MediaMTX は、全部の操作で `authHTTPAddress` に POST する。200 番台なら許可、それ以外は拒否。
API の判定エンドポイント（#10）は `POST /api/internal/mediamtx/auth` で、本文は次の JSON。

```json
{
  "user": "",
  "password": "",
  "token": "",
  "ip": "172.18.0.5",
  "action": "read",
  "path": "cam_<uuid>_sub",
  "protocol": "webrtc",
  "id": "uuid",
  "query": ""
}
```

`action` は `publish` / `read` / `playback` / `api` / `metrics` / `pprof`。

- `authHTTPExclude` は空。Control API（`action: api`）も除外しない
  - Go API は Control API を呼ぶとき、Basic 認証で自分自身を名乗る（ユーザー名とパスワードは API 側の設定）。判定エンドポイントは `action: api` を、この資格情報にだけ許可する（定数時間で比較する）。それ以外の `api`、`metrics`、`pprof`、`publish`、`playback` は拒否する
  - Control API のポート（9997）は外に公開しないので、二重の防御になる
- `action: read` は、ブラウザのセッション Cookie では判定できない（MediaMTX の問い合わせにはブラウザの Cookie が含まれない）。API がライブ用の短命なトークンを発行し、`query`（`?token=...`）または `token` で受け取る方式を #10 で決める
- 判定エンドポイントが落ちている、または到達できないときは、MediaMTX は拒否する（ライブは見えなくなるが、録画は続く）

### セグメント完了の通知

`pathDefaults.runOnRecordSegmentComplete` が `mtx-segment-complete` を実行し、API に POST する。

```
POST /api/internal/mediamtx/segment-complete
Content-Type: application/json
X-VMS-Hook-Token: <VMS_HOOK_TOKEN>

{"path":"cam_<uuid>_main","segment_path":"/recordings/cam_<uuid>_main/2026-01-01_00-00-00-000000.mp4","segment_duration":"1m0.002s"}
```

- API は、トークンを定数時間で比較し、一致しなければ 401 を返す。トークンが未設定のときは、エンドポイント自体を無効にする
- `segment_path` は、API の `/recordings` の下にあることを確認してから使う（`..` を含むものは拒否）。`segment_duration` は MediaMTX の値そのまま（Go の duration 形式の文字列）。パースできないときは ffprobe で調べる
- 通知は `--fail --connect-timeout 2 --max-time 5`、リトライなし。失敗してもスクリプトは常に 0 で終わり、録画には影響しない。取りこぼしは API の起動時と定期のディレクトリ走査が拾う
- 値は jq で JSON にするので、path 名に特殊文字があってもシェルには渡らない。トークンはコマンドライン引数に出さない

### Caddy の `/live/`（#3）

MediaMTX の HLS（8888）と WebRTC（8889）は別のポートで、どちらも path を URL の先頭に置く。プレフィックスを外して振り分ける。

| 外から | 中へ |
|---|---|
| `/live/hls/<path>/index.m3u8` | `mediamtx:8888/<path>/index.m3u8` |
| `/live/webrtc/<path>/whep`、`reader.js` | `mediamtx:8889/<path>/whep` |

```
handle_path /live/hls/*    { reverse_proxy mediamtx:8888 }
handle_path /live/webrtc/* { reverse_proxy mediamtx:8889 }
```

- `Authorization` ヘッダーとクエリは、そのまま通す（MediaMTX が認証の問い合わせに入れる）
- HLS はバッファリングしない（`flush_interval -1`）
- `/live/` の外で Control API（9997）は公開しない
- WebRTC のメディアは UDP/TCP 8189 を直接公開する。公開アドレスは `MTX_WEBRTCADDITIONALHOSTS` で伝える
