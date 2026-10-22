#!/usr/bin/env bash
set -euo pipefail

# Rewrite the version in the Makefile IMG tag, the Helm chart and README.md.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

PROJECT_NAME="plainbucket"
CHART_DIR="${ROOT_DIR}/helm/${PROJECT_NAME}"

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
    printf "  %-35s %s\n" "Chart.yaml version:" "$(sed -n 's/^version: //p' "${CHART_DIR}/Chart.yaml")"
    printf "  %-35s %s\n" "Chart.yaml appVersion:" "$(sed -n 's/^appVersion: //p' "${CHART_DIR}/Chart.yaml")"
    printf "  %-35s %s\n" "values.yaml image.tag:" "$(sed -n 's/^  tag: //p' "${CHART_DIR}/values.yaml")"
    exit 0
fi

NEW_VERSION="$1"

# In a variable: zsh drops the backslashes of an unquoted inline regex.
version_re='^v[0-9]+\.[0-9]+\.[0-9]+$'
if [[ ! "${NEW_VERSION}" =~ ${version_re} ]]; then
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

# Chart versions follow the app version, without the "v".
update_file "${CHART_DIR}/Chart.yaml" \
    "^version: ${CURRENT_VERSION#v}$" \
    "version: ${NEW_VERSION#v}" \
    "Chart.yaml (version)"
update_file "${CHART_DIR}/Chart.yaml" \
    "^appVersion: \"${CURRENT_VERSION}\"$" \
    "appVersion: \"${NEW_VERSION}\"" \
    "Chart.yaml (appVersion)"
# Two-space indent: image.tag only, not other tag keys added later.
update_file "${CHART_DIR}/values.yaml" \
    "^  tag: \"${CURRENT_VERSION}\"$" \
    "  tag: \"${NEW_VERSION}\"" \
    "values.yaml (image.tag)"

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
