#!/usr/bin/env bash
set -euo pipefail

# Rewrite the version in the Makefile IMG tag and README.md.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

PROJECT_NAME="plainbucket"

# Extract current version from Makefile IMG
get_current_version() {
    grep -o "${PROJECT_NAME}:v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*" "${ROOT_DIR}/Makefile" \
        | head -1 \
        | cut -d: -f2
}

if [[ $# -ne 1 ]]; then
    echo "Usage: $0 <new-version>"
    echo "       $0 --current"
    echo "Example: $0 v0.1.0"
    exit 1
fi

if [[ "$1" == "--current" ]]; then
    CURRENT="$(get_current_version)"
    if [[ -z "${CURRENT}" ]]; then
        echo "Error: Could not detect current version from Makefile"
        exit 1
    fi
    echo "Current version: ${CURRENT}"
    echo ""
    echo "Version in each file:"
    printf "  %-35s %s\n" "Makefile:" "${CURRENT}"
    exit 0
fi

NEW_VERSION="$1"

if [[ ! "${NEW_VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Error: Version must be in format vX.Y.Z (e.g., v0.1.0)"
    exit 1
fi

CURRENT_VERSION="$(get_current_version)"
if [[ -z "${CURRENT_VERSION}" ]]; then
    echo "Error: Could not detect current version from Makefile"
    exit 1
fi

if [[ "${CURRENT_VERSION}" == "${NEW_VERSION}" ]]; then
    echo "Already at version ${NEW_VERSION}, nothing to do."
    exit 0
fi

echo "Bumping version: ${CURRENT_VERSION} -> ${NEW_VERSION}"
echo ""

update_file() {
    local file="$1"
    local pattern="$2"
    local replacement="$3"
    local label="$4"

    if [[ ! -f "${file}" ]]; then
        echo "  [SKIP] ${label} (not found)"
        return
    fi

    if ! grep -q -e "${pattern}" "${file}"; then
        echo "  [WARN] ${label} (pattern not found -- version drift?)" >&2
        WARN_COUNT=$(( ${WARN_COUNT:-0} + 1 ))
        return
    fi

    if sed --version >/dev/null 2>&1; then
        sed -i "s|${pattern}|${replacement}|g" "${file}"
    else
        sed -i '' "s|${pattern}|${replacement}|g" "${file}"
    fi
    echo "  [OK]   ${label}"
}

echo "==> Updating required files..."
update_file "${ROOT_DIR}/Makefile" \
    "${PROJECT_NAME}:${CURRENT_VERSION}" \
    "${PROJECT_NAME}:${NEW_VERSION}" \
    "Makefile (IMG)"

echo ""
echo "==> Updating documentation files..."
update_file "${ROOT_DIR}/README.md" \
    "${CURRENT_VERSION}" \
    "${NEW_VERSION}" \
    "README.md"

echo ""
if [[ "${WARN_COUNT:-0}" -gt 0 ]]; then
    echo "" >&2
    echo "WARNING: ${WARN_COUNT} file(s) were NOT updated (pattern not found). Check for version drift above." >&2
fi

echo "Done! Version bumped to ${NEW_VERSION}"
echo ""
echo "Next steps:"
echo "  1. Review changes: git diff"
echo "  2. Commit: git commit -am \"chore: bump version to ${NEW_VERSION}\""
echo "  3. Push: git push origin main"
echo "  4. Build image: make docker-buildx"
echo "  5. Tag: git tag ${NEW_VERSION} && git push origin ${NEW_VERSION}"
