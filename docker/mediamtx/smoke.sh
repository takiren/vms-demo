#!/usr/bin/env bash
# MediaMTX イメージの検査。使い方: smoke.sh <image>
# deploy/mediamtx.yml を使い、認証の問い合わせ先とフックの通知先をスタブにして確認する。
set -euo pipefail
image="${1:?usage: smoke.sh <image>}"
root="$(cd "$(dirname "$0")/../.." && pwd)"
tmp="$(mktemp -d)"
net="mtxsmoke$$"
cleanup() {
  docker rm -f "$net-mtx" "$net-stub" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

# スタブ: /auth は user=vms-api, password=pw の action=api だけ 200、それ以外は 401。
# /hook は受けた内容を記録して 200。
cat > "$tmp/stub.py" <<'PY'
import http.server, json
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(s):
        body = s.rfile.read(int(s.headers.get("Content-Length", 0))).decode()
        if s.path == "/auth":
            d = json.loads(body)
            ok = d.get("action") == "api" and d.get("user") == "vms-api" and d.get("password") == "pw"
            s.send_response(200 if ok else 401)
        else:
            open("/out/hook.log", "a").write(s.headers.get("X-VMS-Hook-Token", "") + "|" + body + "\n")
            s.send_response(200)
        s.end_headers()
    def log_message(*a): pass
http.server.HTTPServer(("0.0.0.0", 8099), H).serve_forever()
PY
mkdir "$tmp/out"; chmod 777 "$tmp/out"

docker network create "$net" >/dev/null
docker run -d --name "$net-stub" --network "$net" -v "$tmp/stub.py:/s.py:ro" -v "$tmp/out:/out" python:3.13-alpine python /s.py >/dev/null
docker run -d --name "$net-mtx" --network "$net" \
  -e MTX_AUTHHTTPADDRESS="http://$net-stub:8099/auth" \
  -e VMS_HOOK_URL="http://$net-stub:8099/hook" -e VMS_HOOK_TOKEN=smoke-token \
  -v "$root/deploy/mediamtx.yml:/etc/mediamtx/mediamtx.yml:ro" "$image" >/dev/null

x() { docker exec "$net-mtx" "$@"; }
for i in $(seq 1 30); do
  x curl -s -o /dev/null localhost:9997/v3/paths/list && break
  sleep 1
done

echo "== non-root"
[ "$(x id -u)" = 10001 ] || { echo "not uid 10001"; exit 1; }
[ "$(x stat -c %u /recordings)" = 10001 ] || { echo "/recordings not owned by 10001"; exit 1; }
echo "== tools"
x ffmpeg -hide_banner -version | head -1
x ffmpeg -hide_banner -L | head -3 | grep -qi lesser || { echo "ffmpeg is not LGPL"; exit 1; }
x curl --version | head -1
x test -f /usr/share/doc/mediamtx/LICENSE
echo "== Control API requires auth"
code=$(x curl -s -o /dev/null -w '%{http_code}' localhost:9997/v3/paths/list)
[ "$code" = 401 ] || { echo "expected 401 without credentials, got $code"; exit 1; }
code=$(x curl -s -u vms-api:pw -o /dev/null -w '%{http_code}' localhost:9997/v3/paths/list)
[ "$code" = 200 ] || { echo "expected 200 with credentials, got $code"; exit 1; }
echo "== no paths defined, path added through Control API"
x curl -s -u vms-api:pw localhost:9997/v3/config/paths/list | grep -q '"itemCount":0' || { echo "paths not empty"; exit 1; }
code=$(x curl -s -u vms-api:pw -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"source":"rtsp://127.0.0.1:1/x","sourceOnDemand":true}' localhost:9997/v3/config/paths/add/cam_smoke_main)
[ "$code" = 200 ] || { echo "path add failed: $code"; exit 1; }
echo "== segment hook"
x env MTX_PATH='a"b$(x)' MTX_SEGMENT_PATH='/recordings/a b.mp4' MTX_SEGMENT_DURATION=1m0.1s /usr/local/bin/mtx-segment-complete
grep -q '^smoke-token|{"path":"a\\"b$(x)","segment_path":"/recordings/a b.mp4","segment_duration":"1m0.1s"}$' "$tmp/out/hook.log" \
  || { echo "unexpected hook payload:"; cat "$tmp/out/hook.log"; exit 1; }
x env VMS_HOOK_URL=http://127.0.0.1:1/x /usr/local/bin/mtx-segment-complete 2>/dev/null \
  || { echo "hook must exit 0 on failure"; exit 1; }
x env -u VMS_HOOK_TOKEN /usr/local/bin/mtx-segment-complete 2>/dev/null \
  || { echo "hook must exit 0 without token"; exit 1; }
echo "OK"
