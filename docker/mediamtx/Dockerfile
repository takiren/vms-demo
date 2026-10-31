# MediaMTX + curl + LGPL ffmpeg.
# ffmpeg は docker/ffmpeg のイメージから取り込む（FFMPEG_IMAGE で指定する）。
#   docker build -t vms-ffmpeg:local docker/ffmpeg
#   docker build -t vms-mediamtx docker/mediamtx
# 別のイメージ名を使うとき:
#   docker build --build-arg FFMPEG_IMAGE=vms-ffmpeg:ci docker/mediamtx
# build context として渡すとき（compose / buildx）:
#   docker buildx build --build-context ffmpeg=docker-image://vms-ffmpeg:ci docker/mediamtx
ARG UBUNTU_VERSION=24.04
ARG MEDIAMTX_VERSION=1.21.3
ARG FFMPEG_IMAGE=vms-ffmpeg:local

FROM bluenviron/mediamtx:${MEDIAMTX_VERSION} AS mediamtx
FROM ${FFMPEG_IMAGE} AS ffmpeg

FROM ubuntu:${UBUNTU_VERSION}

RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates jq \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid 10001 mediamtx \
 && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin mediamtx \
 && mkdir -p /recordings /etc/mediamtx \
 && chown 10001:10001 /recordings

COPY --from=mediamtx /mediamtx /usr/local/bin/mediamtx
COPY --from=ffmpeg /opt/ffmpeg /opt/ffmpeg
COPY LICENSE-mediamtx /usr/share/doc/mediamtx/LICENSE
COPY mtx-segment-complete.sh /usr/local/bin/mtx-segment-complete
RUN chmod 0755 /usr/local/bin/mtx-segment-complete

ENV PATH=/opt/ffmpeg/bin:${PATH}
VOLUME /recordings
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/mediamtx"]
CMD ["/etc/mediamtx/mediamtx.yml"]
