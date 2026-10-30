#!/usr/bin/env bash
# 開発用 compose の結合テスト。リポジトリ直下の .env を使う（なければ deploy/init-env.sh で作る）。
# 使い方: deploy/smoke.sh        （終わったら docker compose down -v する。SMOKE_KEEP=1 で残す）
# ホストの :8080 で API のスタブを動かすので、本物の API は止めておくこと。
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
[ -f .env ] || { echo ".env not found; run deploy/init-env.sh" >&2; exit 1; }
# shellcheck disable=SC1091
set -a; . ./.env; set +a
host="$VMS_HOST"
https_port="${CADDY_HTTPS_PORT:-443}"
tmp="$(mktemp -d)"
dc() { docker compose "$@"; }
stub_pid=""
cleanup() {
  rc=$?
  if [ $rc -ne 0 ]; then
    echo "== FAILED; logs"
    dc ps -a || true
    dc logs --tail 40 || true
  fi
  if [ -n "$stub_pid" ]; then kill "$stub_pid" 2>/dev/null || true; fi
  if [ -z "${SMOKE_KEEP:-}" ]; then
    dc down -v --remove-orphans >/dev/null 2>&1 || true
    dc run --rm --no-deps --entrypoint rm recordings-init -rf /recordings/cam_smoke_main >/dev/null 2>&1 || true
  fi
  rm -rf "$tmp"
  exit $rc
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
# Caddy へは名前解決せずに 127.0.0.1 へ（-k: 内部 CA を信頼しない）
ccurl() { curl -sk --resolve "$host:$https_port:127.0.0.1" --resolve "s3.$host:$https_port:127.0.0.1" "$@"; }
base="https://$host:$https_port"
s3="https://s3.$host:$https_port"
[ "$https_port" = 443 ] && { base="https://$host"; s3="https://s3.$host"; }
mtx() { curl -s -u vms-api:smoke "$@"; }

echo "== start API stub on :8080"
python3 deploy/smoke/stub-api.py 8080 "$tmp/hook.log" &
stub_pid=$!

echo "== docker compose up"
# shellcheck disable=SC2086
dc up -d ${SMOKE_UP_ARGS:---build} --wait --wait-timeout 600

echo "== postgres"
dc exec -T postgres pg_isready -U "$POSTGRES_USER" -d "$POSTGRES_DB" -h 127.0.0.1
dc exec -T postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAc 'select uuidv7()' | grep -Eq '^[0-9a-f-]{36}$' \
  || fail "uuidv7() not available"

echo "== garage-init (first run)"
[ "$(docker inspect -f '{{.State.ExitCode}}' "$(dc ps -aq garage-init)")" = 0 ] || fail "garage-init failed"
echo "== garage-init (second run, idempotent)"
dc run --rm garage-init | tee "$tmp/init2.log"
grep -q 'already has a role' "$tmp/init2.log" || fail "layout re-applied"
grep -q 'already present' "$tmp/init2.log" || fail "key/bucket recreated"

echo "== s3.$host: presigned PUT/GET through Caddy (Host is part of the signature)"
obj="$s3/$S3_BUCKET/smoke/$(date +%s).txt"
echo "hello" > "$tmp/obj.txt"
code=$(ccurl -o /dev/null -w '%{http_code}' -T "$tmp/obj.txt" "$(python3 deploy/smoke/presign.py PUT "$obj")")
[ "$code" = 200 ] || fail "presigned PUT: $code"
[ "$(ccurl "$(python3 deploy/smoke/presign.py GET "$obj")")" = hello ] || fail "presigned GET"
code=$(ccurl -o /dev/null -w '%{http_code}' "$obj")
[ "$code" = 403 ] || fail "anonymous GET should be 403, got $code"
ccurl -o /dev/null -X DELETE "$(python3 deploy/smoke/presign.py DELETE "$obj")"

echo "== CORS"
cors() { ccurl -o /dev/null -D - -X OPTIONS -H "Origin: $1" -H 'Access-Control-Request-Method: GET' \
  -H 'Access-Control-Request-Headers: range' "$s3/$S3_BUCKET/x.mp4" | tr -d '\r'; }
cors "https://$host" > "$tmp/cors.txt"
grep -qi "^access-control-allow-origin: https://$host$" "$tmp/cors.txt" || { cat "$tmp/cors.txt"; fail "CORS: own origin not allowed"; }
if cors "https://evil.example" | grep -qi '^access-control-allow-origin'; then fail "CORS: foreign origin allowed"; fi

echo "== Caddy: /api"
[ "$(ccurl "$base/api/health")" = stub-api ] || fail "/api not routed to the host API"
for p in /api/internal /api/internal/mediamtx/auth /API/Internal/mediamtx/auth /api/internal/../internal/x; do
  code=$(ccurl -o /dev/null -w '%{http_code}' -X POST "$base$p")
  [ "$code" = 404 ] || fail "$p should be blocked, got $code"
done

echo "== ports not published"
for p in 3900 3901 3903 8888 8889; do
  if curl -s -o /dev/null --max-time 2 "http://127.0.0.1:$p/"; then fail "port $p is reachable from the host"; fi
done

echo "== MediaMTX pulls the test streams"
for s in main sub; do
  extra=""; [ $s = main ] && extra=',"record":true'
  code=$(mtx -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    -d "{\"source\":\"rtsp://testsrc:8554/$s\"$extra}" "http://127.0.0.1:9997/v3/config/paths/add/cam_smoke_$s")
  [ "$code" = 200 ] || fail "add path cam_smoke_$s: $code"
done
for s in main sub; do
  for i in $(seq 1 60); do
    mtx "http://127.0.0.1:9997/v3/paths/get/cam_smoke_$s" > "$tmp/$s.json" || true
    python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); sys.exit(0 if d.get("ready") and "H264" in json.dumps(d.get("tracks2") or d.get("tracks")) else 1)' "$tmp/$s.json" 2>/dev/null && break
    [ "$i" = 60 ] && { cat "$tmp/$s.json"; fail "cam_smoke_$s not ready"; }
    sleep 1
  done
  echo "cam_smoke_$s ready"
done

echo "== Caddy: /live/hls and /live/webrtc"
for i in $(seq 1 60); do
  code=$(ccurl -o "$tmp/m3u8" -w '%{http_code}' "$base/live/hls/cam_smoke_sub/index.m3u8")
  [ "$code" = 200 ] && grep -q '#EXTM3U' "$tmp/m3u8" && break
  [ "$i" = 60 ] && fail "HLS via Caddy: $code"
  sleep 1
done
code=$(ccurl -o /dev/null -w '%{http_code}' "$base/live/webrtc/cam_smoke_sub/")
[ "$code" = 200 ] || fail "WebRTC page via Caddy: $code"
code=$(ccurl -o /dev/null -w '%{http_code}' -X OPTIONS "$base/live/webrtc/cam_smoke_sub/whep")
[ "${code:0:1}" = 2 ] || fail "WHEP OPTIONS via Caddy: $code"

echo "== recording lands in deploy/data/recordings and the host can delete it"
for i in $(seq 1 30); do
  f=$(find deploy/data/recordings/cam_smoke_main -name '*.mp4' 2>/dev/null | head -1)
  [ -n "$f" ] && break
  [ "$i" = 30 ] && fail "no recording written"
  sleep 1
done
echo "recorded: $f"
mtx -o /dev/null -X DELETE "http://127.0.0.1:9997/v3/config/paths/delete/cam_smoke_main"
for i in $(seq 1 15); do
  grep -q '"path":"cam_smoke_main"' "$tmp/hook.log" 2>/dev/null && break
  [ "$i" = 15 ] && fail "segment-complete hook not received"
  sleep 1
done
rm -f deploy/data/recordings/cam_smoke_main/*.mp4 || fail "host cannot delete recordings (check MEDIAMTX_UID/GID)"

echo "OK"
