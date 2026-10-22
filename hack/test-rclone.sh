#!/usr/bin/env bash
# End-to-end check with rclone: copy (single and multipart), check by hash, sync with
# deletion, server-side copy and move (multipart too), directory markers and purge.
#
# Usage: hack/test-rclone.sh   (after `make build`; needs rclone)
set -euo pipefail

command -v rclone >/dev/null 2>&1 || { echo "rclone not found" >&2; exit 1; }
# shellcheck source=hack/e2e-lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)/e2e-lib.sh"

export RCLONE_CONFIG="${WORK}/rclone.conf"
export RCLONE_CONFIG_PB_TYPE=s3
export RCLONE_CONFIG_PB_PROVIDER=Other
export RCLONE_CONFIG_PB_ACCESS_KEY_ID="${E2E_ACCESS_KEY}"
export RCLONE_CONFIG_PB_SECRET_ACCESS_KEY="${E2E_SECRET_KEY}"
export RCLONE_CONFIG_PB_ENDPOINT="${ENDPOINT}"
export RCLONE_CONFIG_PB_REGION=us-east-1
export RCLONE_CONFIG_PB_FORCE_PATH_STYLE=true
: >"${RCLONE_CONFIG}"

start_server
rc() { rclone --s3-upload-cutoff 5M --s3-chunk-size 5M "$@"; }

echo "rclone e2e against ${ENDPOINT}"

rc mkdir pb:rcb && ok "mkdir bucket"
[ -d "${DATA}/rcb" ] && ok "bucket is a directory" || fail "bucket directory"

SRC="${WORK}/src"
mkdir -p "${SRC}/docs/deep"
echo "alpha" >"${SRC}/a.txt"
echo "nested" >"${SRC}/docs/deep/n.txt"
head -c $((12 * 1024 * 1024)) /dev/urandom >"${SRC}/big.bin"
rc copy "${SRC}" pb:rcb/tree && ok "copy (12 MiB multipart)"
cmp -s "${SRC}/big.bin" "${DATA}/rcb/tree/big.bin" && ok "multipart object is a plain file" || fail "multipart object on disk"
rc check "${SRC}" pb:rcb/tree >/dev/null 2>&1 && ok "check by hash" || fail "check"
[ "$(rc lsf -R --files-only pb:rcb | wc -l | tr -d ' ')" = "3" ] && ok "lsf -R" || fail "lsf -R"

rm "${SRC}/a.txt"
echo "beta" >"${SRC}/b.txt"
rc sync "${SRC}" pb:rcb/tree && ok "sync"
[ ! -e "${DATA}/rcb/tree/a.txt" ] && [ -f "${DATA}/rcb/tree/b.txt" ] && ok "sync deleted and added files" || fail "sync result"

rc copyto pb:rcb/tree/b.txt pb:rcb/copied.txt && ok "server-side copy"
rc moveto pb:rcb/copied.txt pb:rcb/moved.txt && ok "server-side move"
[ "$(rc cat pb:rcb/moved.txt)" = "beta" ] && [ ! -e "${DATA}/rcb/copied.txt" ] && ok "cat after move" || fail "move result"
rc copyto --s3-copy-cutoff 5M pb:rcb/tree/big.bin pb:rcb/big-copy.bin && cmp -s "${SRC}/big.bin" "${DATA}/rcb/big-copy.bin" &&
    ok "server-side multipart copy (UploadPartCopy)" || fail "server-side multipart copy"

rc mkdir --s3-directory-markers pb:rcb/empty && ok "directory marker"
[ -d "${DATA}/rcb/empty" ] && ok "marker is a directory" || fail "marker on disk"

rc purge pb:rcb && ok "purge"
[ ! -e "${DATA}/rcb" ] && ok "bucket directory removed" || fail "bucket directory left"

echo "all ${PASS} checks passed"
