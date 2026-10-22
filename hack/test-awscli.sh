#!/usr/bin/env bash
# End-to-end check with the real aws cli: starts bin/plainbucket on a temporary root and
# drives mb, cp (single, multipart, server-side), ls, sync, conditional put, public-read,
# a static website with CORS, virtual-hosted requests (via curl --aws-sigv4), rm and rb.
#
# Usage: hack/test-awscli.sh   (after `make build`; needs aws cli v2)
set -euo pipefail

command -v aws >/dev/null 2>&1 || { echo "aws cli not found" >&2; exit 1; }
# shellcheck source=hack/e2e-lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)/e2e-lib.sh"

export AWS_ACCESS_KEY_ID="${E2E_ACCESS_KEY}"
export AWS_SECRET_ACCESS_KEY="${E2E_SECRET_KEY}"
export AWS_DEFAULT_REGION="us-east-1"
export AWS_EC2_METADATA_DISABLED=true
export AWS_CONFIG_FILE=/dev/null
export AWS_SHARED_CREDENTIALS_FILE=/dev/null

export PLAINBUCKET_PUBLIC_BUCKETS="e2e-pub"
export PLAINBUCKET_DOMAINS="localhost"
cat >"${WORK}/config.yaml" <<'EOF_CONFIG'
buckets:
  e2e-site:
    public: true
    website: {error: 404.html}
    cors: {allowed-origins: ["https://app.example.com"]}
EOF_CONFIG
start_server -c "${WORK}/config.yaml"
s3() { aws --endpoint-url "${ENDPOINT}" "$@"; }

echo "aws cli e2e against ${ENDPOINT}"

s3 s3 mb s3://e2e >/dev/null && ok "mb"

head -c 1024 /dev/urandom >"${WORK}/small.bin"
head -c $((20 * 1024 * 1024)) /dev/urandom >"${WORK}/big.bin"
s3 s3 cp "${WORK}/small.bin" "s3://e2e/dir/small file.bin" >/dev/null && ok "cp small"
s3 s3 cp "${WORK}/big.bin" s3://e2e/big.bin >/dev/null && ok "cp 20 MiB (multipart)"

cmp -s "${WORK}/small.bin" "${DATA}/e2e/dir/small file.bin" && ok "small object is a plain file" || fail "small object on disk"
cmp -s "${WORK}/big.bin" "${DATA}/e2e/big.bin" && ok "multipart object is a plain file" || fail "multipart object on disk"

[ "$(s3 s3 ls s3://e2e --recursive | wc -l | tr -d ' ')" = "2" ] && ok "ls --recursive" || fail "ls --recursive"

s3 s3 cp s3://e2e/big.bin "${WORK}/down.bin" >/dev/null
s3 s3 cp s3://e2e/big.bin s3://e2e/big-copy.bin >/dev/null && cmp -s "${WORK}/big.bin" "${DATA}/e2e/big-copy.bin" &&
    ok "server-side copy of 20 MiB (UploadPartCopy)" || fail "server-side copy"
s3 s3 mv s3://e2e/big-copy.bin s3://e2e/big-moved.bin >/dev/null && [ ! -e "${DATA}/e2e/big-copy.bin" ] &&
    cmp -s "${WORK}/big.bin" "${DATA}/e2e/big-moved.bin" && ok "server-side move of 20 MiB" || fail "server-side move"
cmp -s "${WORK}/big.bin" "${WORK}/down.bin" && ok "download" || fail "download"

mkdir -p "${WORK}/site/assets"
echo "<h1>hi</h1>" >"${WORK}/site/index.html"
echo "body{}" >"${WORK}/site/assets/app.css"
s3 s3 sync "${WORK}/site" s3://e2e/site >/dev/null && ok "sync"
[ -z "$(s3 s3 sync "${WORK}/site" s3://e2e/site)" ] && ok "second sync uploads nothing" || fail "second sync re-uploaded"
cmp -s "${WORK}/site/assets/app.css" "${DATA}/e2e/site/assets/app.css" && ok "synced tree is plain files" || fail "synced tree on disk"

s3 s3api put-object --bucket e2e --key lock --body "${WORK}/small.bin" --if-none-match '*' >/dev/null && ok "conditional put creates"
if s3 s3api put-object --bucket e2e --key lock --body "${WORK}/small.bin" --if-none-match '*' >/dev/null 2>"${WORK}/err.txt"; then
    fail "conditional put overwrote"
fi
grep -q PreconditionFailed "${WORK}/err.txt" && ok "conditional put refuses" || fail "conditional put error"

s3 s3 mb s3://e2e-pub >/dev/null || fail "mb e2e-pub"
s3 s3 cp "${WORK}/site/index.html" s3://e2e-pub/index.html >/dev/null || fail "cp to e2e-pub"
s3 --no-sign-request s3 cp s3://e2e-pub/index.html "${WORK}/anon.html" >/dev/null &&
    cmp -s "${WORK}/site/index.html" "${WORK}/anon.html" && ok "anonymous read of a public bucket" || fail "anonymous read"
s3 --no-sign-request s3 ls s3://e2e-pub >/dev/null 2>&1 && fail "anonymous listing allowed" || ok "anonymous listing refused"
s3 --no-sign-request s3 cp s3://e2e/lock "${WORK}/anon.bin" >/dev/null 2>&1 && fail "anonymous read of a private bucket" || ok "private bucket refuses anonymous reads"
s3 s3 rb s3://e2e-pub --force >/dev/null || fail "rb e2e-pub"

# Virtual-hosted style, signed by curl's own SigV4 implementation; --resolve avoids
# depending on the resolver for *.localhost.
vh() { curl -fsS --resolve "e2e.localhost:${PORT}:127.0.0.1" --aws-sigv4 "aws:amz:us-east-1:s3" \
    --user "${E2E_ACCESS_KEY}:${E2E_SECRET_KEY}" "$@"; }
vh -T "${WORK}/small.bin" "http://e2e.localhost:${PORT}/vhost.bin" >/dev/null &&
    cmp -s "${WORK}/small.bin" "${DATA}/e2e/vhost.bin" && ok "virtual-hosted put" || fail "virtual-hosted put"
vh "http://e2e.localhost:${PORT}/vhost.bin" | cmp -s - "${WORK}/small.bin" && ok "virtual-hosted get" || fail "virtual-hosted get"
s3 s3 ls s3://e2e/vhost.bin >/dev/null && ok "path-style sees the virtual-hosted upload" || fail "path-style ls"

mkdir -p "${WORK}/www/blog"
echo "home" >"${WORK}/www/index.html"
echo "blog" >"${WORK}/www/blog/index.html"
echo "gone" >"${WORK}/www/404.html"
s3 s3 mb s3://e2e-site >/dev/null || fail "mb e2e-site"
s3 s3 sync "${WORK}/www" s3://e2e-site >/dev/null || fail "sync e2e-site"
[ "$(curl -fsS "${ENDPOINT}/e2e-site/")" = "home" ] && [ "$(curl -fsSL "${ENDPOINT}/e2e-site/blog")" = "blog" ] &&
    ok "website index documents" || fail "website index"
[ "$(curl -sS -o "${WORK}/404.out" -w '%{http_code}' "${ENDPOINT}/e2e-site/nope")" = "404" ] &&
    [ "$(cat "${WORK}/404.out")" = "gone" ] && ok "website error document" || fail "website error document"
curl -fsS -o /dev/null -D "${WORK}/cors.h" -X OPTIONS -H "Origin: https://app.example.com" \
    -H "Access-Control-Request-Method: GET" "${ENDPOINT}/e2e-site/index.html" &&
    grep -qi "^access-control-allow-origin: https://app.example.com" "${WORK}/cors.h" && ok "CORS preflight" || fail "CORS preflight"
s3 s3 rb s3://e2e-site --force >/dev/null || fail "rb e2e-site"

s3 s3 rm s3://e2e --recursive >/dev/null && ok "rm --recursive"
s3 s3 rb s3://e2e >/dev/null && ok "rb"
[ ! -e "${DATA}/e2e" ] && ok "bucket directory removed" || fail "bucket directory left"

echo "all ${PASS} checks passed"
