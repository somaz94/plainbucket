#!/usr/bin/env bash
# End-to-end check with the real aws cli: starts bin/plainbucket on a temporary root and
# drives mb, cp (single and multipart), ls, sync, conditional put, rm and rb against it.
#
# Usage: hack/test-awscli.sh   (after `make build`; needs aws cli v2)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
BIN="${BIN:-${ROOT_DIR}/bin/plainbucket}"
PORT="${PORT:-19000}"
ENDPOINT="http://127.0.0.1:${PORT}"

command -v aws >/dev/null 2>&1 || { echo "aws cli not found" >&2; exit 1; }
[ -x "${BIN}" ] || { echo "${BIN} not found; run make build" >&2; exit 1; }

WORK="$(mktemp -d)"
DATA="${WORK}/data"
SERVER_PID=""
cleanup() {
    if [ -n "${SERVER_PID}" ]; then
        kill "${SERVER_PID}" 2>/dev/null || true
        wait "${SERVER_PID}" 2>/dev/null || true
    fi
    rm -rf "${WORK}"
}
trap cleanup EXIT

export AWS_ACCESS_KEY_ID="e2e-access"
export AWS_SECRET_ACCESS_KEY="e2e-secret"
export AWS_DEFAULT_REGION="us-east-1"
export AWS_EC2_METADATA_DISABLED=true
export AWS_CONFIG_FILE=/dev/null
export AWS_SHARED_CREDENTIALS_FILE=/dev/null

mkdir -p "${DATA}"
PLAINBUCKET_ROOT="${DATA}" PLAINBUCKET_ADDRESS="127.0.0.1:${PORT}" \
PLAINBUCKET_ACCESS_KEY="${AWS_ACCESS_KEY_ID}" PLAINBUCKET_SECRET_KEY="${AWS_SECRET_ACCESS_KEY}" \
    "${BIN}" >"${WORK}/server.log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 50); do
    curl -fsS "${ENDPOINT}/healthz" >/dev/null 2>&1 && break
    sleep 0.1
done
curl -fsS "${ENDPOINT}/healthz" >/dev/null || { cat "${WORK}/server.log" >&2; exit 1; }

PASS=0
s3() { aws --endpoint-url "${ENDPOINT}" "$@"; }
ok() { PASS=$((PASS + 1)); echo "  ok  $1"; }
fail() { echo "  FAIL $1" >&2; cat "${WORK}/server.log" >&2; exit 1; }

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

s3 s3 rm s3://e2e --recursive >/dev/null && ok "rm --recursive"
s3 s3 rb s3://e2e >/dev/null && ok "rb"
[ ! -e "${DATA}/e2e" ] && ok "bucket directory removed" || fail "bucket directory left"

echo "all ${PASS} checks passed"
