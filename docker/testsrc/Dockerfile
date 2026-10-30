# 開発用のテスト映像を RTSP で配信するイメージ（GPL のコンポーネントを含まない）。
# H.264 のエンコードは OpenH264（BSD-2-Clause）。詳細は README.md。
ARG UBUNTU_VERSION=24.04
FROM ubuntu:${UBUNTU_VERSION}

# gstreamer1.0-plugins-bad は GPL のライブラリ（libfaad2、libdca0 など）に依存するので入れない。
# パッケージを展開して openh264 プラグイン（BSD）の .so だけを取り出す。
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      gstreamer1.0-plugins-base gstreamer1.0-plugins-good gstreamer1.0-x \
      gir1.2-gst-rtsp-server-1.0 libgstrtspserver-1.0-0 python3-gi \
      libopenh264-7 fonts-dejavu-core \
 && cd /tmp \
 && apt-get download gstreamer1.0-plugins-bad \
 && dpkg-deb -x gstreamer1.0-plugins-bad_*.deb bad \
 && cp bad/usr/lib/*/gstreamer-1.0/libgstopenh264.so /usr/lib/"$(uname -m)"-linux-gnu/gstreamer-1.0/ \
 && mkdir -p /usr/share/doc/gst-openh264-plugin \
 && cp bad/usr/share/doc/gstreamer1.0-plugins-bad/copyright /usr/share/doc/gst-openh264-plugin/ \
 && rm -rf /tmp/* /var/lib/apt/lists/* \
 && groupadd --gid 10001 testsrc \
 && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin testsrc

COPY rtsp-server.py /usr/local/bin/rtsp-server
COPY healthcheck.py /usr/local/bin/healthcheck
RUN chmod 0755 /usr/local/bin/rtsp-server /usr/local/bin/healthcheck

USER 10001:10001
EXPOSE 8554
ENTRYPOINT ["/usr/local/bin/rtsp-server"]
