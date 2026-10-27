# shellcheck shell=bash
# Shared by the client end-to-end scripts: starts bin/plainbucket on a temporary root and
# provides check and pinned-download helpers. Source it; it sets WORK, DATA, ENDPOINT and
# an EXIT trap.

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

# start_server [args...]: extra arguments, such as -c <file>, go to the server.
start_server() {
    mkdir -p "${DATA}"
    PLAINBUCKET_ROOT="${DATA}" PLAINBUCKET_ADDRESS="127.0.0.1:${PORT}" \
    PLAINBUCKET_ACCESS_KEY="${E2E_ACCESS_KEY}" PLAINBUCKET_SECRET_KEY="${E2E_SECRET_KEY}" \
        "${BIN}" "$@" >"${WORK}/server.log" 2>&1 &
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

sha256() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

# detect_platform: prints "<os> <arch>" as release download names spell them. It prints rather
# than sets globals, which shellcheck without -x reports as unassigned (SC2154) in the caller.
detect_platform() {
    local os arch
    os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    arch="$(uname -m)"
    case "${arch}" in
        x86_64) arch=amd64 ;;
        aarch64) arch=arm64 ;;
    esac
    echo "${os} ${arch}"
}

# fetch_pinned <file> <sha256> <url> <mode> <label> [curl-option...]: <file> is reused only
# while its hash matches, so a corrupt cache is fetched again. Mode 0644 needs a regular
# file, any other mode an executable one.
fetch_pinned() {
    local file="$1" want="$2" url="$3" mode="$4" label="$5" tmp
    shift 5
    if [ -f "${file}" ] && { [ "${mode}" = 0644 ] || [ -x "${file}" ]; } &&
        [ "$(sha256 "${file}")" = "${want}" ]; then
        return 0
    fi
    tmp="${WORK}/${file##*/}"
    echo "downloading ${url}"
    # Explicit exit: set -e is off when the caller tests this function.
    curl -fsSL "$@" -o "${tmp}" "${url}" || exit
    [ "$(sha256 "${tmp}")" = "${want}" ] || { echo "${label} checksum mismatch" >&2; exit 1; }
    install -m "${mode}" "${tmp}" "${file}"
}
