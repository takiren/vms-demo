# テスト映像（RTSP）

開発と CI 用に、カメラの代わりの RTSP ストリームを 2 本配信するイメージです。

| URL（compose のネットワーク内） | 内容 |
|---|---|
| `rtsp://testsrc:8554/main` | 1280x720 15fps H.264（main 相当）。SMPTE のカラーバーと時刻 |
| `rtsp://testsrc:8554/sub` | 640x360 15fps H.264（sub 相当）。動くボールと時刻 |

ホストからは `rtsp://127.0.0.1:8554/main` で見られます（VLC など）。
解像度は環境変数 `MAIN_SIZE` / `SUB_SIZE`（例 `1920x1080`）、フレームレートは `FPS` で変えられます。

| ファイル | 内容 |
|---|---|
| `Dockerfile` | Ubuntu 24.04 + GStreamer |
| `rtsp-server.py` | gst-rtsp-server（Python / GObject Introspection）で 2 本を配信する |
| `healthcheck.py` | RTSP の DESCRIBE で H.264 の SDP が返るかを確かめる（compose の healthcheck） |

## ライセンス（GPL のコンポーネントを含めない）

開発用でも GPL のエンコーダー（x264 / x265）や GPL の ffmpeg は使いません。
H.264 のエンコードは OpenH264 で行います。

| 使っているもの | パッケージ | ライセンス |
|---|---|---|
| H.264 エンコーダー | `libopenh264-7`（OpenH264） | BSD-2-Clause |
| `openh264enc` | `gstreamer1.0-plugins-bad` の `libgstopenh264.so` だけ | BSD（プラグイン自体の表示）、gst-plugins-bad は LGPL-2.1+ |
| `videotestsrc`、`videoconvert` | `gstreamer1.0-plugins-base` | LGPL-2.1+ |
| `clockoverlay`、`textoverlay` | `gstreamer1.0-x`（pango） | LGPL-2.1+ |
| `rtph264pay` | `gstreamer1.0-plugins-good` | LGPL-2.1+ |
| RTSP サーバー | `libgstrtspserver-1.0-0`、`gir1.2-gst-rtsp-server-1.0` | LGPL-2.1+ |
| Python バインディング | `python3-gi` | LGPL-2.1+ |
| フォント | `fonts-dejavu-core` | Bitstream Vera / パブリックドメイン |

- `gstreamer1.0-plugins-bad` パッケージは丸ごと入れない。GPL のライブラリ（`libfaad2`、`libdca0` など）に依存するため。
  `apt-get download` で取得して展開し、openh264 のプラグインの `.so` だけを取り出す（依存は GStreamer の基本ライブラリと `libopenh264` だけ）
- Ubuntu の `libopenh264-7` はソースからビルドされたもので、Cisco のバイナリ（H.264 の特許ライセンス付き）ではない。開発とテストの用途に限る
- ベースの Ubuntu には、ほかのイメージと同じく GPL のユーザーランド（bash など）が含まれるが、映像のパイプラインはそれにリンクしない

## カメラとして手で登録する（API ができるまでの確認用）

API がまだないので、MediaMTX の Control API に直接 path を足して確かめられます。
Control API は認証を API（`authHTTPAddress`）に問い合わせるので、API かスタブ（`deploy/smoke/stub-api.py`）をホストの :8080 で動かしておきます。

```sh
python3 deploy/smoke/stub-api.py 8080 &   # user=vms-api / password=smoke の api と、read を許可するスタブ
curl -u vms-api:smoke -X POST -H 'Content-Type: application/json' \
  -d '{"source":"rtsp://testsrc:8554/sub"}' http://127.0.0.1:9997/v3/config/paths/add/cam_test_sub
curl -u vms-api:smoke http://127.0.0.1:9997/v3/paths/get/cam_test_sub    # "ready": true になる
open https://vms.localhost/live/webrtc/cam_test_sub/                    # ブラウザで再生（MediaMTX 同梱のページ）
curl -u vms-api:smoke -X DELETE http://127.0.0.1:9997/v3/config/paths/delete/cam_test_sub
```

Control API で足した path は MediaMTX を再起動すると消えます。
