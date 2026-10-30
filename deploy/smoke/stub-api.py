#!/usr/bin/env python3
"""smoke.sh 用の API のスタブ（ホストの :8080 で動かす。本物の API の代わり）。

- POST /api/internal/mediamtx/auth: user=vms-api / password=smoke の action=api と、action=read を許可。他は 401
- POST /api/internal/mediamtx/segment-complete: 記録して 200
- それ以外の /api/*: 200 "stub-api"（Caddy の振り分けの確認用）
"""
import http.server
import json
import sys

log_path = sys.argv[2] if len(sys.argv) > 2 else "/dev/null"


class H(http.server.BaseHTTPRequestHandler):
    def _send(self, code: int, body: bytes = b"") -> None:
        self.send_response(code)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self._send(200, b"stub-api")

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if self.path == "/api/internal/mediamtx/auth":
            d = json.loads(body)
            ok = (d.get("action") == "api" and d.get("user") == "vms-api" and d.get("password") == "smoke") or d.get(
                "action"
            ) == "read"
            self._send(200 if ok else 401)
        elif self.path == "/api/internal/mediamtx/segment-complete":
            with open(log_path, "a") as f:
                f.write(body.decode() + "\n")
            self._send(200)
        else:
            self._send(200, b"stub-api")

    def log_message(self, *a):
        pass


http.server.ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1])), H).serve_forever()
