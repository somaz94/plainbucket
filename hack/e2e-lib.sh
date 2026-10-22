# Shared by the client end-to-end scripts: starts bin/plainbucket on a temporary root and
# provides check helpers. Source it; it sets WORK, DATA, ENDPOINT and an EXIT trap.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/.." && pwd)"
BIN="${BIN:-${ROOT_DIR}/bin/plainbucket}"
PORT="${PORT:-19000}"
ENDPOINT="http://127.0.0.1:${PORT}"
E2E_ACCESS_KEY="e2e-access"
E2E_SECRET_KEY="e2e-secret"

[ -x "${BIN}" ] || { echo "${BIN} not found; run make build" >&2; exit 1; }

WORK="$(mktemp -d)"
DATA="${WORK}/data"
SERVER_PID=""
PASS=0

cleanup() {
    if [ -n "${SERVER_PID}" ]; then
        kill "${SERVER_PID}" 2>/dev/null || true
        wait "${SERVER_PID}" 2>/dev/null || true
    fi
    rm -rf "${WORK:?}"
}
trap cleanup EXIT

start_server() {
    mkdir -p "${DATA}"
    PLAINBUCKET_ROOT="${DATA}" PLAINBUCKET_ADDRESS="127.0.0.1:${PORT}" \
    PLAINBUCKET_ACCESS_KEY="${E2E_ACCESS_KEY}" PLAINBUCKET_SECRET_KEY="${E2E_SECRET_KEY}" \
        "${BIN}" >"${WORK}/server.log" 2>&1 &
    SERVER_PID=$!
    for _ in $(seq 1 50); do
        curl -fsS "${ENDPOINT}/healthz" >/dev/null 2>&1 && return 0
        sleep 0.1
    done
    cat "${WORK}/server.log" >&2
    exit 1
}

ok() { PASS=$((PASS + 1)); echo "  ok  $1"; }
fail() { echo "  FAIL $1" >&2; cat "${WORK}/server.log" >&2; exit 1; }
