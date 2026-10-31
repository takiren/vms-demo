#!/bin/bash
# runOnRecordSegmentComplete から呼ばれ、API にセグメントの完了を通知する。
# 通知に失敗しても録画には影響させない（常に exit 0。取りこぼしは API のディレクトリ走査が拾う）。
# 値は MediaMTX が渡す環境変数から読み、jq で JSON にする（シェルの展開に値を渡さない）。
set -u

url="${VMS_HOOK_URL:-http://host.docker.internal:8080/api/internal/mediamtx/segment-complete}"

if [ -z "${VMS_HOOK_TOKEN:-}" ]; then
  echo "mtx-segment-complete: VMS_HOOK_TOKEN is not set; skipping notification" >&2
  exit 0
fi

# トークンは引数に出さず、ヘッダーをファイル記述子で渡す（ps に残さない）
if jq -n -c \
     --arg path "${MTX_PATH:-}" \
     --arg segment_path "${MTX_SEGMENT_PATH:-}" \
     --arg segment_duration "${MTX_SEGMENT_DURATION:-}" \
     '{path: $path, segment_path: $segment_path, segment_duration: $segment_duration}' \
   | curl --silent --show-error --fail --connect-timeout 2 --max-time 5 \
       --request POST \
       --header 'Content-Type: application/json' \
       --header @<(printf 'X-VMS-Hook-Token: %s\n' "$VMS_HOOK_TOKEN") \
       --data-binary @- \
       -- "$url" >/dev/null; then
  :
else
  echo "mtx-segment-complete: notification failed for ${MTX_SEGMENT_PATH:-?} (API will pick it up by scanning)" >&2
fi
exit 0
