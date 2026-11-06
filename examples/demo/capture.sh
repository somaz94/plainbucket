#!/usr/bin/env bash
# Runs the README demo for real against bin/plainbucket and saves each command's
# output under examples/demo/captured/, which replay.sh prints back in the recording.
# Usage: make build && examples/demo/capture.sh   (needs aws cli v2 and port 9000 free)
set -euo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
ROOT_DIR="$(cd "${DEMO_DIR}/../.." && pwd)"
OUT="${DEMO_DIR}/captured"
ENDPOINT="http://localhost:9000"

command -v aws >/dev/null 2>&1 || { echo "aws cli not found" >&2; exit 1; }
[ -x "${ROOT_DIR}/bin/plainbucket" ] || { echo "bin/plainbucket not found; run make build" >&2; exit 1; }

WORK="$(mktemp -d)"
SERVER_PID=""
cleanup() {
    if [ -n "${SERVER_PID}" ]; then
        kill "${SERVER_PID}" 2>/dev/null || true
        wait "${SERVER_PID}" 2>/dev/null || true
    fi
    rm -rf "${WORK:?}"
}
trap cleanup EXIT

# Keep the user's own aws profile and credentials out of the run.
export AWS_CONFIG_FILE="${WORK}/aws-config" AWS_SHARED_CREDENTIALS_FILE="${WORK}/aws-credentials"
export AWS_ACCESS_KEY_ID=admin AWS_SECRET_ACCESS_KEY=change-me AWS_DEFAULT_REGION=us-east-1

cd "${WORK}"
mkdir -p public/about public/css public/img
printf '<!doctype html>\n<title>Example</title>\n<h1>Hello from a plain file</h1>\n' > public/index.html
printf '<!doctype html>\n<title>About</title>\n<p>About this site.</p>\n' > public/about/index.html
printf 'body { font-family: sans-serif; }\n' > public/css/site.css
printf '<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16"/>\n' > public/img/logo.svg

PLAINBUCKET_ROOT=./data PLAINBUCKET_ACCESS_KEY=admin PLAINBUCKET_SECRET_KEY=change-me \
    "${ROOT_DIR}/bin/plainbucket" > server.log 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 50); do
    curl -fsS "${ENDPOINT}/readyz" >/dev/null 2>&1 && break
    sleep 0.1
done
curl -fsS "${ENDPOINT}/readyz" >/dev/null || { cat server.log >&2; exit 1; }

rm -rf "${OUT}"
mkdir -p "${OUT}"
aws --endpoint-url "${ENDPOINT}" s3 mb s3://site > "${OUT}/01-aws" 2>&1
aws --endpoint-url "${ENDPOINT}" s3 sync ./public s3://site > "${OUT}/02-aws" 2>&1
ls ./data/site > "${OUT}/03-ls" 2>&1
cat ./data/site/index.html > "${OUT}/04-cat" 2>&1

echo "captured into ${OUT}"
