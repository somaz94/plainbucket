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
TMP_VALUES="$(mktemp)"
TMP_DIR="$(mktemp -d)"
trap 'rm -f "${TMP_VALUES}"; rm -rf "${TMP_DIR:?}"' EXIT

echo "helm chart tests"

lint_out="$(helm lint "${CHART}" "${KEY[@]}" 2>&1)" || fail "lint: ${lint_out}"
ok "lint"

render "${KEY[@]}"
has "replicas: 1" "single replica"
has "type: Recreate" "recreate strategy"
has "path: /healthz" "liveness asks /healthz"
has "path: /readyz" "readiness asks /readyz"
has "timeoutSeconds: 3" "readiness timeout above the server's probe"
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
has "value: /etc/plainbucket/credentials/credentials.yaml" "credentials file path"
lacks "subPath: credentials.yaml" "credentials mounted as a directory"
lacks "PLAINBUCKET_ACCESS_KEY" "no full-access key"
ok "credentials secret only"

render "${KEY[@]}" --set "config.publicBuckets={www,assets}" --set "config.domains={s3.example.com}" \
    --set-string config.maxObjectSize=10737418240 --set config.uploadExpiry=24h \
    --set-string config.minFreeSpace=1073741824 --set config.scrubInterval=720h --set-string config.scrubRate=1048576
has 'value: "www,assets"' "public buckets"
has 'value: "720h"' "scrub interval"
has 'value: "1048576"' "scrub rate"
has 'value: "s3.example.com"' "domains"
has 'value: "10737418240"' "max object size kept as digits"
has 'value: "24h"' "upload expiry"
has 'value: "1073741824"' "min free space"
render "${KEY[@]}" --set config.accessLog=true
has 'value: "true"' "access log"
render "${KEY[@]}" --set config.console=true --show-only templates/deployment.yaml
grep -A1 -F "name: PLAINBUCKET_CONSOLE" <<<"${OUT}" | grep -qF 'value: "true"' || fail "console: PLAINBUCKET_CONSOLE not true"
render "${KEY[@]}" --set config.readOnly=true --show-only templates/deployment.yaml
grep -A1 -F "name: PLAINBUCKET_READ_ONLY" <<<"${OUT}" | grep -qF 'value: "true"' || fail "readOnly: PLAINBUCKET_READ_ONLY not true"
render "${KEY[@]}" --set-string config.bodyTimeout=0
has 'value: "0"' "body timeout disabled"
ok "config values"

render "${KEY[@]}" --set config.tlsSecret=pb-tls --set config.metrics=true --set serviceMonitor.enabled=true
has "scheme: HTTPS" "https probes"
has "scheme: https" "https scrape"
has "secretName: pb-tls" "tls volume"
has "kind: ServiceMonitor" "service monitor"
ok "tls and service monitor"

METRICS=(--set config.metrics=true --namespace ns)
render "${KEY[@]}" "${METRICS[@]}" --set prometheusRule.enabled=true --show-only templates/prometheusrule.yaml
has "kind: PrometheusRule" "prometheus rule"
has 'absent(up{job="t-plainbucket", namespace="ns"})' "default selector"
has 'evict="false"' "evicting buckets left out"
rules="${OUT}"
render "${KEY[@]}" "${METRICS[@]}" --set prometheusRule.enabled=true --set prometheusRule.rules.diskLow.enabled=false \
    --set prometheusRule.selector='service="pb"' --set "prometheusRule.additionalRules[0].alert=Extra" \
    --set "prometheusRule.additionalRules[0].expr=vector(1)" --show-only templates/prometheusrule.yaml
lacks "PlainbucketDiskLow" "a disabled rule"
has 'up{service="pb"}' "custom selector"
has "alert: Extra" "additional rules"
if command -v promtool >/dev/null 2>&1; then
    printf '%s\n' "${rules}" | sed -n '/^spec:/,$p' | sed '1d; s/^  //' >"${TMP_DIR}/rules.yaml"
    cp "${ROOT_DIR}/hack/prometheus-rules-test.yaml" "${TMP_DIR}/"
    promtool_out="$(cd "${TMP_DIR}" && { promtool check rules rules.yaml && promtool test rules prometheus-rules-test.yaml; } 2>&1)" ||
        fail "promtool: ${promtool_out}"
    ok "prometheus rule (checked and unit-tested with promtool)"
elif [ -n "${CI:-}" ]; then
    fail "promtool is required in CI"
else
    ok "prometheus rule (promtool not installed: rules not unit-tested)"
fi

render "${KEY[@]}" "${METRICS[@]}" --set grafanaDashboard.enabled=true --set grafanaDashboard.namespace=monitoring \
    --set grafanaDashboard.annotations.grafana_folder=Storage --show-only templates/grafana-dashboard.yaml
has "namespace: monitoring" "dashboard namespace"
has 'grafana_dashboard: "1"' "sidecar label"
has "grafana_folder: Storage" "folder annotation"
has '"uid": "plainbucket"' "dashboard json"
has "name: ns-t-plainbucket-dashboard" "dashboard name carries the release namespace elsewhere"
render "${KEY[@]}" "${METRICS[@]}" --set grafanaDashboard.enabled=true --show-only templates/grafana-dashboard.yaml
has "namespace: ns" "dashboard in the release namespace by default"
has "name: t-plainbucket-dashboard" "dashboard name"
python3 -c 'import json, sys; json.load(open(sys.argv[1]))' "${CHART}/dashboards/plainbucket.json" || fail "dashboard is not valid JSON"
ok "grafana dashboard"

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
has "path: /readyz" "readiness asks /readyz"
lacks "path: /healthz" "liveness path gone with liveness"
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
lacks "args:" "no config file without bucket or rate limit settings"
render -f "${CHART}/examples/runner-cache.yaml" --show-only templates/configmap.yaml
has "max-size: 85899345920" "byte size kept as an integer"
has "expire-after: 336h" "expiry in the config file"
render "${KEY[@]}" --set config.buckets.docs.trash-after=168h --show-only templates/configmap.yaml
has "trash-after: 168h" "trash in the config file"
printf 'config:\n  notifySecret: pb-hooks\n  buckets:\n    uploads:\n      notify:\n        - {url: "https://hooks.example.com/in", events: ["s3:ObjectCreated:*"], secret-file: /etc/plainbucket/notify/key}\n' >"${TMP_VALUES}"
render "${KEY[@]}" -f "${TMP_VALUES}"
has "secret-file: /etc/plainbucket/notify/key" "webhook in the config file"
has "secretName: pb-hooks" "webhook secret volume"
has "mountPath: /etc/plainbucket/notify" "webhook secret mounted as a directory"
lacks "subPath: key" "webhook secret not mounted by subPath"
has "checksum/config:" "webhook changes restart the pod"
render "${KEY[@]}" --show-only templates/deployment.yaml
lacks "/etc/plainbucket/notify" "no webhook secret by default"
render "${KEY[@]}" --set "config.buckets.www.headers[0].suffix=.html" --set "config.buckets.www.headers[0].set.Cache-Control=no-cache" \
    --show-only templates/configmap.yaml
has "Cache-Control: no-cache" "header rule in the config file"
render "${KEY[@]}" --set "config.buckets.ci-cache.lifecycle[0].prefix=logs/" --set "config.buckets.ci-cache.lifecycle[0].delete-after=168h" \
    --show-only templates/configmap.yaml
has "delete-after: 168h" "lifecycle rule in the config file"
render "${KEY[@]}" --set config.rateLimit.requests=50
has "rate-limit:" "rate limit in the config file"
has 'args: ["-c", "/etc/plainbucket/config.yaml"]' "a rate limit alone passes the config file"
has "checksum/config:" "rate limit changes restart the pod"
lacks "buckets:" "no empty buckets map"
lacks "anonymous-rate-limit:" "no anonymous limit unless set"
render "${KEY[@]}" --set config.anonymousRateLimit.requests=5
has "anonymous-rate-limit:" "anonymous rate limit in the config file"
has 'args: ["-c", "/etc/plainbucket/config.yaml"]' "an anonymous rate limit alone passes the config file"
render "${KEY[@]}" --set config.buckets.docs.trash-after=168h --set config.rateLimit.requests=50 --show-only templates/configmap.yaml
has "trash-after: 168h" "buckets kept beside a rate limit"
has "rate-limit:" "rate limit kept beside buckets"
ok "bucket settings"

render "${KEY[@]}" --show-only templates/tests/test-connection.yaml
has "/healthz" "helm test pod"
render "${KEY[@]}" --set config.tlsSecret=pb-tls --show-only templates/tests/test-connection.yaml
has "'-k', 'https://" "helm test pod over https"
lacks "app.kubernetes.io/name" "test pod outside the service selector"
lacks "hook-succeeded" "test pod kept for helm test --logs"
ok "helm test pod"

expect_fail "no access key"
expect_fail "must be set together" --set auth.accessKey=admin
expect_fail "requires config.metrics=true" "${KEY[@]}" --set serviceMonitor.enabled=true
expect_fail "needs config.notifySecret" "${KEY[@]}" --set "config.buckets.uploads.notify[0].url=https://hooks.example.com" \
    --set "config.buckets.uploads.notify[0].secret-file=/etc/plainbucket/notify/key"
expect_fail "requires config.metrics=true" "${KEY[@]}" --set prometheusRule.enabled=true
expect_fail "requires config.metrics=true" "${KEY[@]}" --set grafanaDashboard.enabled=true
expect_fail "empty group" "${KEY[@]}" --set config.metrics=true --set prometheusRule.enabled=true \
    --set prometheusRule.rules.down.enabled=false --set prometheusRule.rules.errors.enabled=false \
    --set prometheusRule.rules.writesRefused.enabled=false --set prometheusRule.rules.diskLow.enabled=false \
    --set prometheusRule.rules.bucketNearLimit.enabled=false --set prometheusRule.rules.notificationsFailing.enabled=false \
    --set prometheusRule.rules.corruptObjects.enabled=false
ok "fail guards"

expect_fail "bogus" "${KEY[@]}" --set bogus=1
expect_fail "replicaCount" "${KEY[@]}" --set replicaCount=2
expect_fail "metrics" "${KEY[@]}" --set config.metrics=yes
expect_fail "accessLog" "${KEY[@]}" --set config.accessLog=yes
expect_fail "console" "${KEY[@]}" --set config.console=yes
expect_fail "readOnly" "${KEY[@]}" --set config.readOnly=yes
expect_fail "publicBuckets" "${KEY[@]}" --set "config.publicBuckets={Bad_Name}"
expect_fail "maxObjectSize" "${KEY[@]}" --set config.maxObjectSize=10Gi
expect_fail "minFreeSpace" "${KEY[@]}" --set-string config.minFreeSpace=1Gi
expect_fail "uploadExpiry" "${KEY[@]}" --set config.uploadExpiry=7d
expect_fail "uploadExpiry" "${KEY[@]}" --set config.uploadExpiry=0s
expect_fail "scrubInterval" "${KEY[@]}" --set config.scrubInterval=30d
expect_fail "scrubRate" "${KEY[@]}" --set-string config.scrubRate=32Mi
expect_fail "maxObjectSize" "${KEY[@]}" --set-string config.maxObjectSize=99999999999999999999
expect_fail "pathType" "${KEY[@]}" --set ingress.enabled=true --set "ingress.hosts[0].host=x" --set "ingress.hosts[0].paths[0].path=/"
expect_fail "terminationGracePeriodSeconds" "${KEY[@]}" --set terminationGracePeriodSeconds=-5
expect_fail "logFormat" "${KEY[@]}" --set config.logFormat=xml
expect_fail "ratio" "${KEY[@]}" --set prometheusRule.rules.diskLow.ratio=2
expect_fail "down" "${KEY[@]}" --set prometheusRule.rules.down.for=soon
expect_fail "release" "${KEY[@]}" --set prometheusRule.labels.release=true
expect_fail "expr" "${KEY[@]}" --set "prometheusRule.additionalRules[0].alert=X"
expect_fail "warn" "${KEY[@]}" --set prometheusRule.rules.down.warn=true
expect_fail "accessMode" "${KEY[@]}" --set persistence.accessMode=RWX
expect_fail "port" "${KEY[@]}" --set service.port=99999
expect_fail "Bad_Name" "${KEY[@]}" --set config.buckets.Bad_Name.public=true
expect_fail "publik" "${KEY[@]}" --set config.buckets.www.publik=true
expect_fail "max-size" "${KEY[@]}" --set config.buckets.cache.max-size=-1
expect_fail "max-size" "${KEY[@]}" --set config.buckets.cache.evict=true
expect_fail "max-size" "${KEY[@]}" --set config.buckets.cache.max-size=100000000000000000000
expect_fail "public" "${KEY[@]}" --set config.buckets.www.website.index=index.html
expect_fail "allowed-origins" "${KEY[@]}" --set "config.buckets.www.cors.allowed-origins={https://*.*.example.com}"
printf 'config:\n  buckets:\n    a..b: {public: true}\n' >"${TMP_VALUES}"
expect_fail "a..b" "${KEY[@]}" -f "${TMP_VALUES}"
expect_fail "expire-after" "${KEY[@]}" --set config.buckets.cache.expire-after=14d
expect_fail "trash-after" "${KEY[@]}" --set config.buckets.docs.trash-after=7d
# Schema messages differ between Helm versions; a valid bucket name keeps the key name specific.
expect_fail "url" "${KEY[@]}" --set "config.buckets.uploads.notify[0].events={s3:ObjectCreated:*}"
expect_fail "url" "${KEY[@]}" --set "config.buckets.uploads.notify[0].url=ftp://h"
expect_fail "url" "${KEY[@]}" --set "config.buckets.uploads.notify[0].url=http://user@"
expect_fail "events" "${KEY[@]}" --set "config.buckets.uploads.notify[0].url=http://h" --set "config.buckets.uploads.notify[0].events={s3:ObjectTagging:*}"
expect_fail "secret-file" "${KEY[@]}" --set "config.buckets.uploads.notify[0].url=http://h" --set "config.buckets.uploads.notify[0].secret-file=key"
expect_fail "secret" "${KEY[@]}" --set "config.buckets.uploads.notify[0].url=http://h" --set "config.buckets.uploads.notify[0].secret=x"
expect_fail "Content-Type" "${KEY[@]}" --set "config.buckets.www.headers[0].set.Content-Type=text/html"
expect_fail "set" "${KEY[@]}" --set "config.buckets.www.headers[0].prefix=a/"
expect_fail "delete-after" "${KEY[@]}" --set "config.buckets.ci-cache.lifecycle[0].delete-after=7d"
expect_fail "requests" "${KEY[@]}" --set config.rateLimit.requests=-1
expect_fail "rps" "${KEY[@]}" --set config.anonymousRateLimit.rps=5
expect_fail "bytes" "${KEY[@]}" --set-string config.rateLimit.bytes=10Mi
expect_fail "burst" "${KEY[@]}" --set-json 'config.anonymousRateLimit={"burst":1.5}'
expect_fail "expire" "${KEY[@]}" --set "config.buckets.ci-cache.lifecycle[0].expire=1h"
printf 'config:\n  buckets:\n    www:\n      headers:\n        - set: {Cache-Control: "a\\nb"}\n' >"${TMP_VALUES}"
expect_fail "Cache-Control" "${KEY[@]}" -f "${TMP_VALUES}"
expect_fail "timeoutSeconds" "${KEY[@]}" --set probes.readiness.timeoutSeconds=2
expect_fail "allowed-methods" "${KEY[@]}" --set "config.buckets.www.cors.allowed-origins={*}" --set "config.buckets.www.cors.allowed-methods={PATCH}"
ok "schema rejects invalid values"

for f in "${CHART}"/examples/*.yaml; do
    render -f "${f}"
    lint_out="$(helm lint "${CHART}" -f "${f}" 2>&1)" || fail "lint ${f}: ${lint_out}"
done
ok "examples render"

echo "all ${PASS} checks passed"
