#!/usr/bin/env bash
# .env.example から .env を作り、CHANGE_ME をランダムな値で埋める。
# 使い方: deploy/init-env.sh [出力先（既定: リポジトリ直下の .env）]
# 既存のファイルは上書きしない（秘密情報を消さないため）。
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
out="${1:-$root/.env}"
if [ -e "$out" ]; then
  echo "$out already exists; not overwriting" >&2
  exit 1
fi
command -v openssl >/dev/null || { echo "openssl is required" >&2; exit 1; }

hex() { openssl rand -hex "$1"; }
pg_password="$(hex 24)"
umask 077
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in
    VMS_SECRET_KEY=CHANGE_ME)       line="VMS_SECRET_KEY=$(openssl rand -base64 32)" ;;
    POSTGRES_PASSWORD=CHANGE_ME)    line="POSTGRES_PASSWORD=$pg_password" ;;
    DATABASE_URL=*CHANGE_ME*)       line="${line/CHANGE_ME/$pg_password}" ;;
    GARAGE_RPC_SECRET=CHANGE_ME)    line="GARAGE_RPC_SECRET=$(hex 32)" ;;
    S3_ACCESS_KEY_ID=CHANGE_ME)     line="S3_ACCESS_KEY_ID=GK$(hex 12)" ;;
    S3_SECRET_ACCESS_KEY=CHANGE_ME) line="S3_SECRET_ACCESS_KEY=$(hex 32)" ;;
    *=CHANGE_ME)                    line="${line%=CHANGE_ME}=$(hex 32)" ;;
  esac
  printf '%s\n' "$line"
done < "$root/.env.example" > "$out"
if grep -q CHANGE_ME "$out"; then
  echo "warning: CHANGE_ME left in $out" >&2
fi
echo "wrote $out"
