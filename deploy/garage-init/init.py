#!/usr/bin/env python3
"""Garage の初期設定を冪等に行う（compose の garage-init コンテナ）。

1. layout: このノードに役割がなければ割り当てて適用する
2. キー: .env の S3_ACCESS_KEY_ID / S3_SECRET_ACCESS_KEY を取り込む
3. バケット: S3_BUCKET（recordings）を作り、キーに read / write を許可する
4. CORS: https://<VMS_HOST> からの GET / HEAD だけを許可する

何度実行しても同じ状態になる。外部ライブラリは使わない（Python の標準ライブラリだけ）。
Garage の Admin API v2 と、CORS の設定にだけ S3 API（PutBucketCors、SigV4 署名）を使う。
"""
from __future__ import annotations

import base64
import datetime
import hashlib
import hmac
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from xml.sax.saxutils import escape

KEY_ID_RE = re.compile(r"^GK[0-9a-f]{24}$")
SECRET_RE = re.compile(r"^[0-9a-f]{64}$")
BUCKET_RE = re.compile(r"^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$")
HOST_RE = re.compile(r"^[A-Za-z0-9.-]+(:[0-9]{1,5})?$")
# ブラウザが録画のセグメント（presigned URL）を取得するのに要るもの
CORS_METHODS = ["GET", "HEAD"]
# Garage は Access-Control-Request-Headers を大文字小文字を区別して照合する。ブラウザは小文字で送るので小文字で書く
CORS_ALLOWED_HEADERS = ["range", "if-none-match", "if-modified-since"]
CORS_EXPOSE_HEADERS = ["Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"]
CORS_MAX_AGE = 3600


class InitError(Exception):
    pass


def log(msg: str) -> None:
    print(f"garage-init: {msg}", flush=True)


def env(name: str, default: str | None = None) -> str:
    v = os.environ.get(name, default)
    if v is None or v == "":
        raise InitError(f"{name} is not set")
    return v


def load_config() -> dict:
    c = {
        "admin": env("GARAGE_ADMIN_URL", "http://garage:3903").rstrip("/"),
        "s3": env("GARAGE_S3_URL", "http://garage:3900").rstrip("/"),
        "region": env("S3_REGION", "garage"),
        "token": env("GARAGE_ADMIN_TOKEN"),
        "key_id": env("S3_ACCESS_KEY_ID"),
        "secret": env("S3_SECRET_ACCESS_KEY"),
        "bucket": env("S3_BUCKET", "recordings"),
        "host": env("VMS_HOST"),
        "zone": env("GARAGE_ZONE", "dc1"),
        "capacity": env("GARAGE_CAPACITY", "100G"),
        "key_name": env("GARAGE_KEY_NAME", "vms-api"),
    }
    validate(c)
    c["capacity_bytes"] = parse_size(c["capacity"])
    return c


def validate(c: dict) -> None:
    if c["token"] == "CHANGE_ME":
        raise InitError("GARAGE_ADMIN_TOKEN is still CHANGE_ME; run deploy/init-env.sh or set it in .env")
    # Garage が自分で作るキーと同じ形式に限る（Garage 自体はもっと緩いが、.env の誤りを早く見つけるため）
    if not KEY_ID_RE.match(c["key_id"]):
        raise InitError(
            "S3_ACCESS_KEY_ID must be 'GK' followed by 24 lowercase hex chars "
            "(e.g. GK$(openssl rand -hex 12)); got %r" % c["key_id"]
        )
    if not SECRET_RE.match(c["secret"]):
        raise InitError("S3_SECRET_ACCESS_KEY must be 64 lowercase hex chars (openssl rand -hex 32)")
    if not BUCKET_RE.match(c["bucket"]):
        raise InitError("S3_BUCKET is not a valid bucket name: %r" % c["bucket"])
    if not HOST_RE.match(c["host"]):
        raise InitError("VMS_HOST must be a host name (no scheme or path): %r" % c["host"])


def parse_size(s: str) -> int:
    m = re.fullmatch(r"([0-9]+)([KMGT]?)", s.strip().upper())
    if not m:
        raise InitError(f"GARAGE_CAPACITY must look like 100G: {s!r}")
    return int(m.group(1)) * 1000 ** "_KMGT".index(m.group(2) or "_")


def cors_origin(host: str) -> str:
    return f"https://{host}"


# ---- Admin API v2 ----------------------------------------------------------


class Admin:
    def __init__(self, base: str, token: str):
        self.base = base
        self.token = token

    def call(self, method: str, endpoint: str, query: dict | None = None, body=None, ok404=False):
        url = f"{self.base}/v2/{endpoint}"
        if query:
            url += "?" + urllib.parse.urlencode(query)
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(url, data=data, method=method)
        req.add_header("Authorization", f"Bearer {self.token}")
        if data is not None:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                raw = r.read()
                return json.loads(raw) if raw else None
        except urllib.error.HTTPError as e:
            if ok404 and e.code == 404:
                return None
            detail = e.read().decode(errors="replace")[:500]
            raise InitError(f"{method} {endpoint}: HTTP {e.code}: {detail}") from None


def wait_admin(admin: Admin, timeout: float = 120) -> dict:
    deadline = time.monotonic() + timeout
    while True:
        try:
            return admin.call("GET", "GetClusterStatus")
        except (InitError, OSError) as e:
            if isinstance(e, InitError) and ("HTTP 401" in str(e) or "HTTP 403" in str(e)):
                raise InitError("Admin API rejected GARAGE_ADMIN_TOKEN") from None
            if time.monotonic() > deadline:
                raise InitError(f"Garage admin API not reachable: {e}") from None
            time.sleep(2)


def ensure_layout(admin: Admin, c: dict) -> None:
    status = wait_admin(admin)
    nodes = status.get("nodes") or []
    if len(nodes) != 1:
        raise InitError(f"expected exactly one Garage node, found {len(nodes)}")
    node_id = nodes[0]["id"]
    layout = admin.call("GET", "GetClusterLayout")
    if any(r.get("id") == node_id for r in layout.get("roles") or []):
        log(f"layout: node {node_id[:16]} already has a role (version {layout['version']})")
        return
    staged = layout.get("stagedRoleChanges") or []
    if not any(r.get("id") == node_id for r in staged):
        admin.call(
            "POST",
            "UpdateClusterLayout",
            body={"roles": [{"id": node_id, "zone": c["zone"], "capacity": c["capacity_bytes"], "tags": []}]},
        )
    admin.call("POST", "ApplyClusterLayout", body={"version": layout["version"] + 1})
    log(f"layout: assigned node {node_id[:16]} zone={c['zone']} capacity={c['capacity']} and applied")


def wait_healthy(admin: Admin, timeout: float = 60) -> None:
    deadline = time.monotonic() + timeout
    while True:
        h = admin.call("GET", "GetClusterHealth")
        if h.get("status") == "healthy":
            return
        if time.monotonic() > deadline:
            raise InitError(f"cluster not healthy: {h}")
        time.sleep(1)


def ensure_key(admin: Admin, c: dict) -> None:
    info = admin.call("GET", "GetKeyInfo", {"id": c["key_id"], "showSecretKey": "true"}, ok404=True)
    if info is None:
        admin.call(
            "POST",
            "ImportKey",
            body={"accessKeyId": c["key_id"], "secretAccessKey": c["secret"], "name": c["key_name"]},
        )
        log(f"key: imported {c['key_id']}")
        return
    if not hmac.compare_digest(info.get("secretAccessKey") or "", c["secret"]):
        raise InitError(
            f"key {c['key_id']} already exists in Garage with a different secret; "
            "restore the original S3_SECRET_ACCESS_KEY or delete the key (garage key delete) first"
        )
    log(f"key: {c['key_id']} already present")


def ensure_bucket(admin: Admin, c: dict) -> str:
    info = admin.call("GET", "GetBucketInfo", {"globalAlias": c["bucket"]}, ok404=True)
    if info is None:
        info = admin.call("POST", "CreateBucket", body={"globalAlias": c["bucket"]})
        log(f"bucket: created {c['bucket']}")
    else:
        log(f"bucket: {c['bucket']} already present")
    return info["id"]


def set_permissions(admin: Admin, bucket_id: str, key_id: str, owner: bool) -> None:
    admin.call(
        "POST",
        "AllowBucketKey",
        body={"bucketId": bucket_id, "accessKeyId": key_id, "permissions": {"read": True, "write": True, "owner": False}},
    )
    # owner は CORS を設定する間だけ与える。API に要るのは read / write だけ
    admin.call(
        "POST",
        "AllowBucketKey" if owner else "DenyBucketKey",
        body={"bucketId": bucket_id, "accessKeyId": key_id, "permissions": {"read": False, "write": False, "owner": True}},
    )


# ---- S3（SigV4）-------------------------------------------------------------


def s3_request(c: dict, method: str, path: str, query: str = "", body: bytes = b"") -> tuple[int, bytes]:
    u = urllib.parse.urlsplit(c["s3"])
    host = u.netloc
    now = datetime.datetime.now(datetime.timezone.utc)
    amz_date = now.strftime("%Y%m%dT%H%M%SZ")
    date = now.strftime("%Y%m%d")
    payload_hash = hashlib.sha256(body).hexdigest()
    headers = {"host": host, "x-amz-content-sha256": payload_hash, "x-amz-date": amz_date}
    if body:
        headers["content-md5"] = base64.b64encode(hashlib.md5(body).digest()).decode()
        headers["content-type"] = "application/xml"
    signed = ";".join(sorted(headers))
    canonical = "\n".join(
        [method, path, query, "".join(f"{k}:{headers[k]}\n" for k in sorted(headers)), signed, payload_hash]
    )
    scope = f"{date}/{c['region']}/s3/aws4_request"
    to_sign = "\n".join(["AWS4-HMAC-SHA256", amz_date, scope, hashlib.sha256(canonical.encode()).hexdigest()])
    k = ("AWS4" + c["secret"]).encode()
    for part in (date, c["region"], "s3", "aws4_request"):
        k = hmac.new(k, part.encode(), hashlib.sha256).digest()
    sig = hmac.new(k, to_sign.encode(), hashlib.sha256).hexdigest()
    headers["authorization"] = f"AWS4-HMAC-SHA256 Credential={c['key_id']}/{scope}, SignedHeaders={signed}, Signature={sig}"
    url = f"{c['s3']}{path}" + (f"?{query}" if query else "")
    req = urllib.request.Request(url, data=body or None, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def cors_xml(origin: str) -> bytes:
    def tags(name: str, values: list[str]) -> str:
        return "".join(f"<{name}>{escape(v)}</{name}>" for v in values)

    rule = (
        "<CORSRule>"
        + tags("AllowedOrigin", [origin])
        + tags("AllowedMethod", CORS_METHODS)
        + tags("AllowedHeader", CORS_ALLOWED_HEADERS)
        + tags("ExposeHeader", CORS_EXPOSE_HEADERS)
        + f"<MaxAgeSeconds>{CORS_MAX_AGE}</MaxAgeSeconds>"
        + "</CORSRule>"
    )
    return (
        '<?xml version="1.0" encoding="UTF-8"?>'
        '<CORSConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">' + rule + "</CORSConfiguration>"
    ).encode()


def ensure_cors(c: dict) -> None:
    path = "/" + urllib.parse.quote(c["bucket"])
    origin = cors_origin(c["host"])
    status, body = s3_request(c, "PUT", path, "cors=", cors_xml(origin))
    if status != 200:
        raise InitError(f"PutBucketCors: HTTP {status}: {body[:500]!r}")
    status, body = s3_request(c, "GET", path, "cors=")
    if status != 200 or f"<AllowedOrigin>{escape(origin)}</AllowedOrigin>".encode() not in body:
        raise InitError(f"GetBucketCors did not return the expected rule: HTTP {status}: {body[:500]!r}")
    log(f"cors: {c['bucket']} allows {origin} ({', '.join(CORS_METHODS)})")


def main() -> int:
    try:
        c = load_config()
        admin = Admin(c["admin"], c["token"])
        ensure_layout(admin, c)
        wait_healthy(admin)
        ensure_key(admin, c)
        bucket_id = ensure_bucket(admin, c)
        set_permissions(admin, bucket_id, c["key_id"], owner=True)
        try:
            ensure_cors(c)
        finally:
            set_permissions(admin, bucket_id, c["key_id"], owner=False)
        log(f"key {c['key_id']}: read/write on {c['bucket']} (owner revoked)")
    except InitError as e:
        print(f"garage-init: error: {e}", file=sys.stderr, flush=True)
        return 1
    log("done")
    return 0


if __name__ == "__main__":
    sys.exit(main())
