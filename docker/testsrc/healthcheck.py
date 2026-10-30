#!/usr/bin/env python3
"""RTSP の DESCRIBE が 200 を返し、SDP が H.264 なら成功（compose の healthcheck 用）。"""
import socket
import sys

path = sys.argv[1] if len(sys.argv) > 1 else "main"
url = f"rtsp://127.0.0.1:8554/{path}"
req = f"DESCRIBE {url} RTSP/1.0\r\nCSeq: 1\r\nAccept: application/sdp\r\n\r\n".encode()
try:
    with socket.create_connection(("127.0.0.1", 8554), timeout=5) as s:
        s.sendall(req)
        data = b""
        while b"H264" not in data and len(data) < 65536:
            chunk = s.recv(4096)
            if not chunk:
                break
            data += chunk
except OSError as e:
    sys.exit(f"unhealthy: {e}")
if not data.startswith(b"RTSP/1.0 200") or b"H264" not in data:
    sys.exit(f"unhealthy: {data[:200]!r}")
