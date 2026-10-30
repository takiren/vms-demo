# ffmpeg（LGPL ビルド）

API（と MediaMTX のイメージ）が使う ffmpeg / ffprobe を、LGPL-2.1-or-later の最小構成でビルドします。
用途はサムネイルの生成と、録画セグメントの結合（コピーのみ）です。トランスコードはしません。

| ファイル | 内容 |
|---|---|
| `Dockerfile` | Ubuntu 24.04 ベースの multi-stage ビルド |
| `configure-flags.txt` | `configure` に渡す引数の全部（このファイルが正） |
| `verify.sh` | ライセンス、構成、動作の検査（イメージの中で実行する） |
| `ffmpeg-release-key.asc` | ソースの署名を検証する公開鍵 |
| `testdata/` | 検査用の小さな fMP4（下記） |

## ソース

| 項目 | 値 |
|---|---|
| バージョン | FFmpeg 9.0.2 |
| URL | https://ffmpeg.org/releases/ffmpeg-9.0.2.tar.xz |
| 署名 | https://ffmpeg.org/releases/ffmpeg-9.0.2.tar.xz.asc |
| SHA-256 | `8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e` |
| 署名の鍵 | FFmpeg release signing key（`FCF986EA15E6E293A5644F10B4322F04D67658D8`）。https://ffmpeg.org/ffmpeg-devel.asc から取得したものを `ffmpeg-release-key.asc` として固定 |

ビルドの中で SHA-256 と GPG 署名の両方を検証します。ソースへの変更（パッチ）はありません。
ビルドしたイメージの `/opt/ffmpeg/src/` にも、同じ tarball と署名を入れています。

バージョンを上げるときは、`Dockerfile` の `FFMPEG_VERSION` と `FFMPEG_SHA256` と、この表を一緒に直します。

## ビルド

```sh
docker build -t vms-ffmpeg docker/ffmpeg
docker run --rm --entrypoint /opt/ffmpeg-test/verify.sh vms-ffmpeg
```

Dockerfile の中でしていることは次のとおりです（Docker を使わずに同じものを作る手順でもあります）。

1. Ubuntu 24.04 に `build-essential nasm pkg-config` を入れる
2. 上記のソースを取得して検証し、展開する
3. `configure-flags.txt` のコメントと空行を除いた引数で `./configure` を実行する
4. `make && make install`
5. `config.h` で `CONFIG_GPL` / `CONFIG_NONFREE` / `CONFIG_VERSION3` が 0 であることを確認する
6. ヘッダーと pkg-config のファイルを除き、バイナリと共有ライブラリを strip する

### 構成

`--disable-everything` で全部無効にしてから、必要なものだけを有効にしています。
`--disable-autodetect` なので、外部ライブラリには一切リンクしません（依存は glibc だけ）。

| 種類 | 有効にするもの | 用途 |
|---|---|---|
| デコーダー | `h264`, `hevc` | サムネイル（先頭のキーフレーム） |
| デコーダー | `wrapped_avframe` | 検査用（lavfi の入力） |
| エンコーダー | `mjpeg` | サムネイル |
| demuxer | `mov`, `concat` | fMP4 セグメントの読み込みと結合 |
| muxer | `mp4`, `mov`, `image2`, `image2pipe`, `mjpeg`, `null` | 切り出しの mp4、サムネイル |
| parser | `h264`, `hevc`, `aac`, `opus`, `mjpeg` | ストリームコピー |
| bsf | `h264_mp4toannexb`, `hevc_mp4toannexb`, `aac_adtstoasc`, `extract_extradata`, `null` | ストリームコピー |
| フィルター | `scale`, `format`, `select`, `null`, `anull`, `testsrc` | サムネイルの縮小。`testsrc` は検査用 |
| プロトコル | `file`, `pipe` | ネットワークは無効（`--disable-network`） |
| 入力デバイス | `lavfi` | 検査用 |

音声はコピーだけなので、音声のデコーダーは入れていません。

## イメージのレイアウト

```
/opt/ffmpeg/bin/ffmpeg, ffprobe   RUNPATH=/opt/ffmpeg/lib（--enable-rpath）
/opt/ffmpeg/lib/lib*.so*          libavcodec, libavformat, libavfilter, libavdevice, libavutil, libswscale, libswresample
/opt/ffmpeg/share/doc/ffmpeg/     COPYING.LGPLv2.1, LICENSE.md, CREDITS, configure-flags.txt, BUILDINFO
/opt/ffmpeg/src/                  ソースの tarball と署名
/opt/ffmpeg-test/                 verify.sh と testdata（このイメージだけ）
```

- 単体で使う: `docker run --rm -v "$PWD:/work" -w /work vms-ffmpeg -i in.mp4 ...`（ENTRYPOINT は `ffmpeg`、ユーザーは uid 10001）
- 他のイメージに取り込む:

  ```dockerfile
  FROM vms-ffmpeg AS ffmpeg
  FROM ubuntu:24.04
  COPY --from=ffmpeg /opt/ffmpeg /opt/ffmpeg
  ENV PATH=/opt/ffmpeg/bin:$PATH
  ```

  `/opt/ffmpeg` をそのまま同じパスにコピーしてください。RUNPATH があるので `ldconfig` や `LD_LIBRARY_PATH` は要りません。
  ライセンスとソースも一緒に入ります。
  取り込む側は glibc 2.39 以上（Ubuntu 24.04 相当）が必要です。Alpine（musl）では動きません。

## LGPL への対応

FFmpeg のこの構成は LGPL-2.1-or-later です（`ffmpeg -L` で確認できます）。

- GPL のコンポーネント（`--enable-gpl`）と nonfree のコンポーネント（`--enable-nonfree`）は有効にしない。x264 / x265 / fdk-aac は使わない
- `--enable-version3`（LGPLv3 化）もしない
- 共有ライブラリとしてビルドし、イメージに同梱する
- 対応するソース（上記の tarball。変更なし）とビルド手順（このディレクトリ）を公開する。イメージの中にも同梱する
- このリポジトリの API は ffmpeg を CLI として exec で呼ぶだけで、ライブラリにリンクしない

### ライブラリの差し替え

利用者は、改変したライブラリに差し替えて動かせます。

1. 上記のソースを改変し、同じ `configure-flags.txt`（`--prefix` は任意）でビルドする。ABI が同じなら `libav*.so.<major>` の差し替えで動く
2. 次のどちらかで読み込ませる
   - `/opt/ffmpeg/lib/` の該当ファイルを置き換える
   - 別のディレクトリに置いて `LD_LIBRARY_PATH` で指定する（RUNPATH より優先される）

   ```sh
   docker run --rm -v "$PWD/mylib:/mylib:ro" -e LD_LIBRARY_PATH=/mylib vms-ffmpeg -version
   ```

ABI を変える改変なら、`ffmpeg` / `ffprobe` も同じソースからビルドし直してください。

## テスト用のデータ

`testdata/` の fMP4 は、検査で h264 / hevc のデコードと結合を試すためのものです（それぞれ 1 秒、160x90）。
このイメージには H.264 / HEVC のエンコーダーがないので、別の ffmpeg（libx264 / libx265 入り）で作ってコミットしています。
ファイルはデータとしてだけ使い、エンコーダーはイメージに含まれません。

```sh
M="-movflags +frag_keyframe+empty_moov+default_base_moof -map_metadata -1 -fflags +bitexact"
for i in 0 1; do
  ffmpeg -f lavfi -i testsrc=size=160x90:rate=10:duration=1 -f lavfi -i sine=frequency=440:duration=1 \
    -c:v libx264 -preset veryfast -g 10 -pix_fmt yuv420p -c:a aac -b:a 32k $M h264_aac_$i.mp4
done
ffmpeg -f lavfi -i testsrc=size=160x90:rate=10:duration=1 \
  -c:v libx265 -preset veryfast -g 10 -pix_fmt yuv420p -tag:v hvc1 $M hevc_0.mp4
```
