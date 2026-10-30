#!/usr/bin/env python3
"""開発用のテスト映像を RTSP で配信する。

rtsp://<host>:8554/main  1280x720 15fps H.264（main 相当）
rtsp://<host>:8554/sub    640x360 15fps H.264（sub 相当）

環境変数で解像度とフレームレートを変えられる（MAIN_SIZE=1920x1080 など）。
"""
import os
import signal

import gi

gi.require_version("Gst", "1.0")
gi.require_version("GstRtspServer", "1.0")
from gi.repository import GLib, Gst, GstRtspServer  # noqa: E402


def pipeline(name: str, size: str, fps: int, bitrate: int, pattern: str) -> str:
    w, h = (int(v) for v in size.split("x"))
    return (
        f"( videotestsrc is-live=true pattern={pattern} "
        f"! video/x-raw,width={w},height={h},framerate={fps}/1 "
        f'! clockoverlay time-format="%Y-%m-%d %H:%M:%S" halignment=left valignment=top '
        f'! textoverlay text="{name}" halignment=right valignment=top '
        f"! videoconvert ! video/x-raw,format=I420 "
        f"! openh264enc bitrate={bitrate} gop-size={fps * 2} rate-control=bitrate complexity=low "
        f"! video/x-h264,profile=constrained-baseline "
        f"! rtph264pay name=pay0 pt=96 config-interval=1 )"
    )


def main() -> None:
    Gst.init(None)
    fps = int(os.environ.get("FPS", "15"))
    streams = {
        "main": pipeline("main", os.environ.get("MAIN_SIZE", "1280x720"), fps, 2_000_000, "smpte"),
        "sub": pipeline("sub", os.environ.get("SUB_SIZE", "640x360"), fps, 500_000, "ball"),
    }
    server = GstRtspServer.RTSPServer()
    server.set_service(os.environ.get("RTSP_PORT", "8554"))
    mounts = server.get_mount_points()
    for name, launch in streams.items():
        f = GstRtspServer.RTSPMediaFactory()
        f.set_launch(launch)
        f.set_shared(True)  # 同じストリームを複数のクライアントで共有する
        mounts.add_factory(f"/{name}", f)
    if server.attach(None) == 0:
        raise SystemExit("failed to bind RTSP port")
    loop = GLib.MainLoop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        GLib.unix_signal_add(GLib.PRIORITY_DEFAULT, sig, loop.quit)
    print(f"serving rtsp://0.0.0.0:{server.get_service()}/{{main,sub}}", flush=True)
    loop.run()


if __name__ == "__main__":
    main()
