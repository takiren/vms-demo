# syntax=docker/dockerfile:1
#
# LGPL-2.1-or-later な最小構成の ffmpeg / ffprobe をビルドする。
# 手順とライセンス上の注意は docker/ffmpeg/README.md を参照。
#
# 成果物のレイアウト（他のイメージからは `COPY --from=<this> /opt/ffmpeg /opt/ffmpeg` で取り込む）:
#   /opt/ffmpeg/bin/{ffmpeg,ffprobe}   RUNPATH=/opt/ffmpeg/lib
#   /opt/ffmpeg/lib/lib*.so*           共有ライブラリ（LGPL。差し替え可能）
#   /opt/ffmpeg/share/doc/ffmpeg/      ライセンス、configure の引数、ビルド情報
#   /opt/ffmpeg/src/                   対応するソース tarball と署名

ARG UBUNTU_VERSION=24.04

FROM ubuntu:${UBUNTU_VERSION} AS build

ARG FFMPEG_VERSION=9.0.2
ARG FFMPEG_SHA256=8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e
# "FFmpeg release signing key <ffmpeg-devel@ffmpeg.org>"
ARG FFMPEG_GPG_FINGERPRINT=FCF986EA15E6E293A5644F10B4322F04D67658D8

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      build-essential ca-certificates curl gnupg nasm pkg-config xz-utils \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# ソースを取得し、SHA-256（固定値）と GPG 署名（固定したフィンガープリントの鍵）の両方で検証する
RUN set -eux; \
    curl -fsSLO "https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz"; \
    curl -fsSLO "https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz.asc"; \
    curl -fsSL -o ffmpeg-devel.asc https://ffmpeg.org/ffmpeg-devel.asc; \
    echo "${FFMPEG_SHA256}  ffmpeg-${FFMPEG_VERSION}.tar.xz" | sha256sum -c -; \
    export GNUPGHOME="$(mktemp -d)"; \
    gpg --batch --import ffmpeg-devel.asc; \
    gpg --batch --status-fd 1 --verify "ffmpeg-${FFMPEG_VERSION}.tar.xz.asc" "ffmpeg-${FFMPEG_VERSION}.tar.xz" \
      | grep -E "^\[GNUPG:\] VALIDSIG ${FFMPEG_GPG_FINGERPRINT} "; \
    rm -rf "${GNUPGHOME}"; \
    tar -xJf "ffmpeg-${FFMPEG_VERSION}.tar.xz"

COPY configure-flags.txt /build/configure-flags.txt

# --disable-autodetect で、ビルド環境にあるライブラリを勝手に取り込まないようにする。
# 有効にするコンポーネントは configure-flags.txt に列挙する（README にも同じものを載せる）。
RUN set -eux; \
    cd "ffmpeg-${FFMPEG_VERSION}"; \
    grep -vE '^\s*(#|$)' /build/configure-flags.txt | xargs ./configure; \
    make -j"$(nproc)"; \
    make install; \
    # GPL / nonfree が有効になっていないことをビルド時点でも確認する
    if grep -E 'CONFIG_(GPL|NONFREE|GPLV3|VERSION3) 1' config.h; then echo 'GPL/nonfree enabled' >&2; exit 1; fi; \
    grep -q 'CONFIG_SHARED 1' config.h; \
    doc=/opt/ffmpeg/share/doc/ffmpeg; \
    mkdir -p "$doc" /opt/ffmpeg/src; \
    cp COPYING.LGPLv2.1 LICENSE.md CREDITS "$doc/"; \
    cp /build/configure-flags.txt "$doc/"; \
    printf 'ffmpeg %s\nsha256 %s\nsource https://ffmpeg.org/releases/ffmpeg-%s.tar.xz\n' \
      "${FFMPEG_VERSION}" "${FFMPEG_SHA256}" "${FFMPEG_VERSION}" > "$doc/BUILDINFO"; \
    cp "/build/ffmpeg-${FFMPEG_VERSION}.tar.xz" "/build/ffmpeg-${FFMPEG_VERSION}.tar.xz.asc" /opt/ffmpeg/src/; \
    # 実行時に不要なヘッダーと pkg-config を除き、実体のファイルだけ strip する（シンボリックリンクを壊さない）
    rm -rf /opt/ffmpeg/include /opt/ffmpeg/lib/pkgconfig /opt/ffmpeg/share/ffmpeg; \
    find /opt/ffmpeg/bin /opt/ffmpeg/lib -type f -exec strip --strip-unneeded {} +

FROM ubuntu:${UBUNTU_VERSION} AS runtime

LABEL org.opencontainers.image.title="vms-demo ffmpeg (LGPL)" \
      org.opencontainers.image.description="Minimal LGPL-2.1-or-later build of FFmpeg for vms-demo" \
      org.opencontainers.image.licenses="LGPL-2.1-or-later" \
      org.opencontainers.image.source="https://github.com/takiren/vms-demo"

COPY --from=build /opt/ffmpeg/bin /opt/ffmpeg/bin
COPY --from=build /opt/ffmpeg/lib /opt/ffmpeg/lib
COPY --from=build /opt/ffmpeg/share/doc /opt/ffmpeg/share/doc
COPY --from=build /opt/ffmpeg/src /opt/ffmpeg/src
# 検査用（/opt/ffmpeg の外に置き、COPY --from で取り込む側には持ち込まない）
COPY verify.sh /opt/ffmpeg-test/verify.sh
COPY testdata /opt/ffmpeg-test/testdata

RUN useradd --uid 10001 --user-group --no-create-home --shell /usr/sbin/nologin ffmpeg

ENV PATH=/opt/ffmpeg/bin:${PATH}
USER 10001:10001
WORKDIR /tmp
ENTRYPOINT ["ffmpeg"]
CMD ["-hide_banner", "-buildconf"]
