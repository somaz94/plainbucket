#!/usr/bin/env bash
# End-to-end check of the web console: its files and headers, and its SigV4 signer
# (driven from node) against the server's verifier.
#
# Usage: hack/test-console.sh   (after `make build`; needs node >= 18)
set -euo pipefail

command -v node >/dev/null 2>&1 || { echo "node not found" >&2; exit 1; }
# shellcheck source=hack/e2e-lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)/e2e-lib.sh"

cat >"${WORK}/creds.yaml" <<'EOF_CREDS'
- {access-key: console-scoped, secret-key: console-scoped-secret, buckets: [other]}
EOF_CREDS
PLAINBUCKET_CONSOLE=true PLAINBUCKET_CREDENTIALS_FILE="${WORK}/creds.yaml" start_server

headers="$(curl -fsS -D - -o "${WORK}/index.html" "${ENDPOINT}/_console/")" || fail "console page fetch"
grep -qF 'app.js' "${WORK}/index.html" || fail "console index"
grep -qi "^content-security-policy: default-src 'none'" <<<"${headers}" || fail "console CSP"
grep -qi '^cross-origin-opener-policy: same-origin' <<<"${headers}" || fail "console opener policy"
grep -qi '^x-frame-options: DENY' <<<"${headers}" || fail "console framing"
ok "console page and headers"
[ "$(curl -s -o /dev/null -w '%{http_code}' "${ENDPOINT}/_console/api/usage")" = 403 ] || fail "anonymous usage api"
ok "usage api needs a signature"

ENDPOINT="${ENDPOINT}" ACCESS_KEY="${E2E_ACCESS_KEY}" SECRET_KEY="${E2E_SECRET_KEY}" \
    node "${ROOT_DIR}/hack/test-console.mjs" || fail "console signer"
echo "all $((PASS)) shell checks passed"
