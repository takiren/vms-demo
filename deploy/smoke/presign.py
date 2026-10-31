#!/usr/bin/env python3
"""SigV4 の presigned URL を作る（smoke.sh 用。標準ライブラリだけ）。

usage: presign.py METHOD URL   （S3_ACCESS_KEY_ID / S3_SECRET_ACCESS_KEY / S3_REGION を環境変数で）
"""
import datetime
import hashlib
import hmac
import os
import sys
import urllib.parse

method, url = sys.argv[1], sys.argv[2]
key_id, secret = os.environ["S3_ACCESS_KEY_ID"], os.environ["S3_SECRET_ACCESS_KEY"]
region = os.environ.get("S3_REGION", "garage")
u = urllib.parse.urlsplit(url)
now = datetime.datetime.now(datetime.timezone.utc)
amz_date, date = now.strftime("%Y%m%dT%H%M%SZ"), now.strftime("%Y%m%d")
scope = f"{date}/{region}/s3/aws4_request"
q = {
    "X-Amz-Algorithm": "AWS4-HMAC-SHA256",
    "X-Amz-Credential": f"{key_id}/{scope}",
    "X-Amz-Date": amz_date,
    "X-Amz-Expires": "300",
    "X-Amz-SignedHeaders": "host",
}
qs = "&".join(f"{urllib.parse.quote(k, safe='')}={urllib.parse.quote(v, safe='')}" for k, v in sorted(q.items()))
canonical = "\n".join([method, u.path, qs, f"host:{u.netloc}\n", "host", "UNSIGNED-PAYLOAD"])
to_sign = "\n".join(["AWS4-HMAC-SHA256", amz_date, scope, hashlib.sha256(canonical.encode()).hexdigest()])
k = ("AWS4" + secret).encode()
for part in (date, region, "s3", "aws4_request"):
    k = hmac.new(k, part.encode(), hashlib.sha256).digest()
sig = hmac.new(k, to_sign.encode(), hashlib.sha256).hexdigest()
print(f"{u.scheme}://{u.netloc}{u.path}?{qs}&X-Amz-Signature={sig}")
