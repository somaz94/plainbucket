#!/usr/bin/env bash
# Lint and template-test the Helm chart: renders, the fail guards, schema rejections and
# every file in examples/.
#
# Usage: hack/test-helm.sh   (or make test-helm; needs helm)
set -euo pipefail

command -v helm >/dev/null 2>&1 || { echo "helm not found" >&2; exit 1; }
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/.." && pwd)"
CHART="${ROOT_DIR}/helm/plainbucket"
PASS=0
OUT=""

ok() { PASS=$((PASS + 1)); echo "  ok  $1"; }
fail() { echo "  FAIL $1" >&2; exit 1; }

# render <helm template args...>: renders into OUT, failing the test if helm fails.
render() { OUT="$(helm template t "${CHART}" "$@" 2>&1)" || fail "render failed ($*): ${OUT}"; }
has() { grep -qF -- "$1" <<<"${OUT}" || fail "$2: missing '$1'"; }
lacks() { if grep -qF -- "$1" <<<"${OUT}"; then fail "$2: unexpected '$1'"; fi; }

# expect_fail <error text> <helm template args...>: the render must fail, for that reason.
expect_fail() {
    local want=$1 err
    shift
    if err="$(helm template t "${CHART}" "$@" 2>&1 >/dev/null)"; then
        fail "rendered but should not: $*"
    fi
    grep -qF -- "${want}" <<<"${err}" || fail "failed for the wrong reason ($*): ${err}"
}

KEY=(--set auth.existingSecret=pb-auth)

echo "helm chart tests"

lint_out="$(helm lint "${CHART}" "${KEY[@]}" 2>&1)" || fail "lint: ${lint_out}"
ok "lint"

render "${KEY[@]}"
has "replicas: 1" "single replica"
has "type: Recreate" "recreate strategy"
has "name: pb-auth" "existing secret"
has "claimName: t-plainbucket-data" "chart-created claim"
has "helm.sh/resource-policy: keep" "claim kept on uninstall"
has "automountServiceAccountToken: false" "no service account token"
has "readOnlyRootFilesystem: true" "read-only root"
has "type: RuntimeDefault" "seccomp"
lacks "kind: Secret" "no chart secret with an existing one"
lacks "PLAINBUCKET_CREDENTIALS_FILE" "no credentials file by default"
lacks "kind: Ingress" "no ingress by default"
ok "default render"

render --set auth.accessKey=admin --set auth.secretKey=change-me
has "kind: Secret" "chart-created secret"
has "name: t-plainbucket-auth" "chart secret name"
has "checksum/auth:" "secret checksum annotation"
lacks "value: change-me" "secret value in the pod spec"
ok "chart-created secret"

render --set auth.credentialsSecret=pb-creds
has "PLAINBUCKET_CREDENTIALS_FILE" "credentials file env"
has "secretName: pb-creds" "credentials volume"
has "subPath: credentials.yaml" "credentials mount"
lacks "PLAINBUCKET_ACCESS_KEY" "no full-access key"
ok "credentials secret only"

render "${KEY[@]}" --set "config.publicBuckets={www,assets}" --set "config.domains={s3.example.com}" \
    --set-string config.maxObjectSize=10737418240 --set config.uploadExpiry=24h
has 'value: "www,assets"' "public buckets"
has 'value: "s3.example.com"' "domains"
has 'value: "10737418240"' "max object size kept as digits"
has 'value: "24h"' "upload expiry"
render "${KEY[@]}" --set-string config.bodyTimeout=0
has 'value: "0"' "body timeout disabled"
ok "config values"

render "${KEY[@]}" --set config.tlsSecret=pb-tls --set config.metrics=true --set serviceMonitor.enabled=true
has "scheme: HTTPS" "https probes"
has "scheme: https" "https scrape"
has "secretName: pb-tls" "tls volume"
has "kind: ServiceMonitor" "service monitor"
ok "tls and service monitor"

render "${KEY[@]}" --set persistence.enabled=false
has "emptyDir: {}" "emptyDir without persistence"
lacks "kind: PersistentVolumeClaim" "no claim without persistence"
render "${KEY[@]}" --set persistence.existingClaim=data --set persistence.subPath=objects
has "claimName: data" "existing claim"
has "subPath: objects" "data subPath"
lacks "kind: PersistentVolumeClaim" "no claim with an existing one"
ok "storage modes"

render "${KEY[@]}" --set probes.liveness.enabled=false --set resources=null --show-only templates/deployment.yaml
lacks "livenessProbe" "liveness disabled"
has "readinessProbe" "readiness kept"
lacks "resources:" "resources removed"
ok "optional pod fields"

render "${KEY[@]}" -f "${CHART}/examples/static-site.yaml"
has 'host: "*.s3.example.com"' "wildcard ingress host"
ok "ingress"

render "${KEY[@]}" -f "${CHART}/examples/static-site.yaml" --show-only templates/configmap.yaml
has "error: 404.html" "bucket settings in the config file"
render "${KEY[@]}" -f "${CHART}/examples/static-site.yaml" --show-only templates/deployment.yaml
has 'args: ["-c", "/etc/plainbucket/config.yaml"]' "config file argument"
has "checksum/config:" "config checksum annotation"
render "${KEY[@]}" --show-only templates/deployment.yaml
lacks "args:" "no config file without bucket settings"
ok "bucket settings"

render "${KEY[@]}" --show-only templates/tests/test-connection.yaml
has "/healthz" "helm test pod"
render "${KEY[@]}" --set config.tlsSecret=pb-tls --show-only templates/tests/test-connection.yaml
has "'-k', 'https://" "helm test pod over https"
lacks "app.kubernetes.io/name" "test pod outside the service selector"
ok "helm test pod"

expect_fail "no access key"
expect_fail "must be set together" --set auth.accessKey=admin
expect_fail "requires config.metrics=true" "${KEY[@]}" --set serviceMonitor.enabled=true
ok "fail guards"

expect_fail "bogus" "${KEY[@]}" --set bogus=1
expect_fail "replicaCount" "${KEY[@]}" --set replicaCount=2
expect_fail "metrics" "${KEY[@]}" --set config.metrics=yes
expect_fail "publicBuckets" "${KEY[@]}" --set "config.publicBuckets={Bad_Name}"
expect_fail "maxObjectSize" "${KEY[@]}" --set config.maxObjectSize=10Gi
expect_fail "uploadExpiry" "${KEY[@]}" --set config.uploadExpiry=7d
expect_fail "uploadExpiry" "${KEY[@]}" --set config.uploadExpiry=0s
expect_fail "maxObjectSize" "${KEY[@]}" --set-string config.maxObjectSize=99999999999999999999
expect_fail "pathType" "${KEY[@]}" --set ingress.enabled=true --set "ingress.hosts[0].host=x" --set "ingress.hosts[0].paths[0].path=/"
expect_fail "terminationGracePeriodSeconds" "${KEY[@]}" --set terminationGracePeriodSeconds=-5
expect_fail "logFormat" "${KEY[@]}" --set config.logFormat=xml
expect_fail "accessMode" "${KEY[@]}" --set persistence.accessMode=RWX
expect_fail "port" "${KEY[@]}" --set service.port=99999
expect_fail "Bad_Name" "${KEY[@]}" --set config.buckets.Bad_Name.public=true
expect_fail "publik" "${KEY[@]}" --set config.buckets.www.publik=true
expect_fail "allowed-methods" "${KEY[@]}" --set "config.buckets.www.cors.allowed-origins={*}" --set "config.buckets.www.cors.allowed-methods={PATCH}"
ok "schema rejects invalid values"

for f in "${CHART}"/examples/*.yaml; do
    render -f "${f}"
    lint_out="$(helm lint "${CHART}" -f "${f}" 2>&1)" || fail "lint ${f}: ${lint_out}"
done
ok "examples render"

echo "all ${PASS} checks passed"
