#!/bin/sh
# ffmpeg イメージの検査。イメージの中で実行する:
#   docker run --rm --entrypoint /opt/ffmpeg-test/verify.sh <image>
# 1. ライセンス: LGPL で、GPL / nonfree / version3 が有効になっていない
# 2. 構成: プロジェクトが使うコンポーネントがそろっている
# 3. 動作: fMP4 からのサムネイル生成と、セグメントの結合（コピーのみ）
set -eu

TESTDATA="${TESTDATA:-$(dirname "$0")/testdata}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok: $*"; }

FF="ffmpeg -hide_banner -nostdin -loglevel error"

# --- 1. ライセンス -----------------------------------------------------------
buildconf="$(ffmpeg -hide_banner -buildconf)"
for bad in --enable-gpl --enable-nonfree --enable-version3; do
  if printf '%s\n' "$buildconf" | grep -q -- "$bad"; then
    fail "buildconf contains $bad"
  fi
done
for want in --disable-gpl --disable-nonfree --enable-shared --disable-autodetect; do
  printf '%s\n' "$buildconf" | grep -q -- "$want" || fail "buildconf lacks $want"
done
ok "buildconf has no GPL/nonfree/version3"

license="$(ffmpeg -hide_banner -L)"
printf '%s\n' "$license" | grep -q 'GNU Lesser General Public' || fail "ffmpeg -L is not LGPL"
if printf '%s\n' "$license" | grep -q -e 'GNU General Public License' -e 'nonfree' -e 'unredistributable'; then
  fail "ffmpeg -L mentions GPL or nonfree"
fi
ok "ffmpeg -L reports LGPL"

# 各ライブラリが自分のライセンスを LGPL と申告しているか（ffprobe の -L も同じ構成）
ffprobe -hide_banner -L | grep -q 'GNU Lesser General Public' || fail "ffprobe -L is not LGPL"
ok "ffprobe -L reports LGPL"

# 外部ライブラリにリンクしていない（libc 系とランタイムの ffmpeg の共有ライブラリだけ）
if command -v ldd >/dev/null 2>&1; then
  for bin in /opt/ffmpeg/bin/ffmpeg /opt/ffmpeg/bin/ffprobe; do
    unexpected="$(ldd "$bin" | awk '{print $1}' \
      | grep -vE '^(linux-vdso|/lib.*/ld-linux|ld-linux|libc\.so|libm\.so|libpthread\.so|libdl\.so|librt\.so|libav[a-z]+\.so|libsw[a-z]+\.so)' || true)"
    [ -z "$unexpected" ] || fail "$bin links unexpected libraries: $unexpected"
    if ldd "$bin" | grep -q 'not found'; then fail "$bin has unresolved libraries"; fi
    if ldd "$bin" | grep -E 'libav|libsw' | grep -vq '/opt/ffmpeg/lib/'; then
      fail "$bin resolves ffmpeg libraries outside /opt/ffmpeg/lib"
    fi
  done
  ok "binaries link only libc and /opt/ffmpeg/lib"
fi

# --- 2. 構成 -----------------------------------------------------------------
# $1: ffmpeg のオプション（-decoders など）、$2: 行の先頭のフラグの正規表現、$3...: 名前
require() {
  opt="$1"; flags="$2"; shift 2
  list="$(ffmpeg -hide_banner "$opt")"
  for name in "$@"; do
    printf '%s\n' "$list" | grep -qE "^ *${flags}${flags:+ +}${name}( |$)" || fail "$opt lacks $name"
  done
  ok "$opt: $*"
}
require -decoders '[A-Z.]{6}' h264 hevc
require -encoders '[A-Z.]{6}' mjpeg
require -muxers '[A-Z. ]{2,3}' mp4 mov image2 image2pipe mjpeg
require -demuxers '[A-Z. ]{2,3}' mov,mp4,m4a,3gp,3g2,mj2 concat
require -bsfs '' h264_mp4toannexb hevc_mp4toannexb aac_adtstoasc
require -filters '[A-Z.]{2,3}' scale format select
require -protocols '' file pipe

# GPL の外部エンコーダーが入っていない
if ffmpeg -hide_banner -encoders | grep -qE 'libx26[45]|libfdk'; then
  fail "GPL/nonfree encoder present"
fi
ok "no libx264/libx265/libfdk_aac"

# --- 3. 動作 -----------------------------------------------------------------
# lavfi の testsrc から JPEG を作る（mjpeg エンコーダーと image2 muxer）
$FF -f lavfi -i testsrc=size=160x90:rate=1 -frames:v 1 -update 1 "$WORK/testsrc.jpg"
[ -s "$WORK/testsrc.jpg" ] || fail "testsrc -> jpeg"
ok "testsrc -> mjpeg"

# fMP4（h264 / hevc）の先頭のキーフレームから縮小したサムネイルを作る
for src in h264_aac_0.mp4 hevc_0.mp4; do
  $FF -skip_frame nokey -i "$TESTDATA/$src" -frames:v 1 -vf scale=80:-2 -update 1 "$WORK/${src%.mp4}.jpg"
  [ -s "$WORK/${src%.mp4}.jpg" ] || fail "thumbnail from $src"
  ok "thumbnail from $src"
done

# パイプへの出力（API は標準出力から読む）
size="$($FF -skip_frame nokey -i "$TESTDATA/hevc_0.mp4" -frames:v 1 -f image2pipe -c:v mjpeg pipe:1 | wc -c)"
[ "$size" -gt 0 ] || fail "thumbnail to pipe"
ok "thumbnail to pipe ($size bytes)"

# セグメントを concat demuxer で結合し、コピーのみで mp4 にする（映像 + 音声）
printf "file '%s'\nfile '%s'\n" "$TESTDATA/h264_aac_0.mp4" "$TESTDATA/h264_aac_1.mp4" > "$WORK/list.txt"
$FF -f concat -safe 0 -i "$WORK/list.txt" -c copy -movflags +faststart "$WORK/clip.mp4"
streams="$(ffprobe -v error -show_entries stream=codec_name -of csv=p=0 "$WORK/clip.mp4" | sort | tr '\n' ' ')"
[ "$streams" = "aac h264 " ] || fail "concat streams: '$streams'"
duration="$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$WORK/clip.mp4")"
awk -v d="$duration" 'BEGIN { exit !(d >= 1.8 && d <= 2.6) }' || fail "concat duration $duration"
ok "concat + stream copy -> mp4 ($streams, ${duration}s)"

# 断片化した mp4 を標準出力へ（シークできない出力でも書ける）
size="$($FF -i "$TESTDATA/h264_aac_0.mp4" -c copy -f mp4 -movflags frag_keyframe+empty_moov pipe:1 | wc -c)"
[ "$size" -gt 0 ] || fail "fragmented mp4 to pipe"
ok "fragmented mp4 to pipe ($size bytes)"

echo "all checks passed"
