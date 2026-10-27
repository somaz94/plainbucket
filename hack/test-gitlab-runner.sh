#!/usr/bin/env bash
# End-to-end check with the official gitlab-runner binary: its cache helper commands
# upload and download a job cache through plainbucket the three ways a runner does it,
# with presigned URLs (with and without the HEAD existence checks) and with a Go Cloud
# s3:// URL and credentials.
#
# Usage: hack/test-gitlab-runner.sh   (after `make build`; needs go, curl and unzip)
# The verified binary is kept in bin/ so later runs skip the download.
set -euo pipefail

GLR_VERSION="19.5.0"

# shellcheck source=hack/e2e-lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)/e2e-lib.sh"

read -r os arch <<<"$(detect_platform)"
# From https://gitlab-runner-downloads.s3.amazonaws.com/v${GLR_VERSION}/release.sha256
case "${os}-${arch}" in
    linux-amd64) want=f5f3322cb5482e1176b159730717b5ee37fb6eb8cbf195065b35062e02825579 ;;
    linux-arm64) want=941ff1634546341fe7215a63f7f8960b7cd153d17b193cec3688072f4f4ffe8b ;;
    darwin-amd64) want=c0b346ba37b26faa51f5fbc35c40917fc099a799f318d4b3ae2c797a39eea694 ;;
    darwin-arm64) want=be05a21191878dbd3900d48111e86d58fea324b21a451c533c95d366346333dc ;;
    *) echo "no pinned gitlab-runner checksum for ${os}-${arch}" >&2; exit 1 ;;
esac

GLR="${ROOT_DIR}/bin/gitlab-runner-${GLR_VERSION}-${os}-${arch}"
fetch_pinned "${GLR}" "${want}" \
    "https://gitlab-runner-downloads.s3.amazonaws.com/v${GLR_VERSION}/binaries/gitlab-runner-${os}-${arch}" 0755 gitlab-runner

(cd "${ROOT_DIR}/compat" && go build -o "${WORK}/presign" ./cmd/presign)

export AWS_ACCESS_KEY_ID="${E2E_ACCESS_KEY}"
export AWS_SECRET_ACCESS_KEY="${E2E_SECRET_KEY}"
export AWS_REGION="us-east-1"
export AWS_EC2_METADATA_DISABLED=true
export AWS_CONFIG_FILE=/dev/null
export AWS_SHARED_CREDENTIALS_FILE=/dev/null

start_server
echo "gitlab-runner ${GLR_VERSION} cache e2e against ${ENDPOINT}"

presign() { "${WORK}/presign" "${ENDPOINT}" "$1" runner-cache "$2"; }
# The helper prints colored progress; only failures are shown.
glr() { "${GLR}" "$@" >"${WORK}/glr.log" 2>&1 || { cat "${WORK}/glr.log" >&2; return 1; }; }

curl -fsS --aws-sigv4 "aws:amz:us-east-1:s3" --user "${E2E_ACCESS_KEY}:${E2E_SECRET_KEY}" \
    -X PUT "${ENDPOINT}/runner-cache" >/dev/null && ok "create bucket" || fail "create bucket"

JOB="${WORK}/job"
mkdir -p "${JOB}/vendor"
head -c 300000 /dev/urandom >"${JOB}/vendor/lib.bin"
echo "cached" >"${JOB}/vendor/notes.txt"
cp -R "${JOB}/vendor" "${WORK}/expected"
cd "${JOB}"

key="project/1/default-protected"
put="$(presign PUT "${key}")"
get="$(presign GET "${key}")"
head="$(presign HEAD "${key}")"

glr cache-archiver --file ../cache.zip --path vendor --url "${put}" && ok "archiver uploads with a presigned PUT" || fail "archiver upload"
unzip -l "${DATA}/runner-cache/${key}" | grep "vendor/lib.bin" >/dev/null && ok "cache archive is a plain zip file" || fail "cache archive on disk"

glr cache-archiver --file ../cache.zip --path vendor --url "${put}" --check-url "${head}" || fail "archiver with head check"
grep -q "already exists remotely" "${WORK}/glr.log" && ok "archiver skips an existing cache (presigned HEAD)" || fail "archiver re-uploaded"

rm -rf vendor ../cache.zip
glr cache-extractor --file ../cache.zip --url "${get}" && diff -r "${WORK}/expected" vendor >/dev/null &&
    ok "extractor downloads with a presigned GET" || fail "extractor download"

rm -rf vendor
glr cache-extractor --file ../cache.zip --url "${get}" --head-url "${head}" && diff -r "${WORK}/expected" vendor >/dev/null &&
    ok "extractor with a presigned HEAD check" || fail "extractor with head check"

rm -rf vendor ../cache.zip
gocloud="s3://runner-cache/project/2/gocloud?endpoint=${ENDPOINT}&use_path_style=true&region=us-east-1&awssdk=v2"
cp -R "${WORK}/expected" vendor
glr cache-archiver --file ../gocloud.zip --path vendor --gocloud-url "${gocloud}" && ok "archiver uploads with a Go Cloud URL" || fail "Go Cloud upload"
[ -f "${DATA}/runner-cache/project/2/gocloud" ] && ok "Go Cloud upload is a plain file" || fail "Go Cloud upload on disk"
rm -rf vendor ../gocloud.zip
glr cache-extractor --file ../gocloud.zip --gocloud-url "${gocloud}" && diff -r "${WORK}/expected" vendor >/dev/null &&
    ok "extractor downloads with a Go Cloud URL" || fail "Go Cloud download"

echo "all ${PASS} checks passed"
