#!/usr/bin/env bash
# Runs a subset of ceph/s3-tests against bin/plainbucket and compares the passing tests
# with hack/s3tests-pass.txt: a listed test that no longer passes fails the run, a new
# pass is only reported. S3TESTS_UPDATE=1 rewrites the list from this run instead.
#
# Usage: hack/test-s3tests.sh   (after `make build`; needs git and uv or python3 >= 3.10)
set -euo pipefail

# shellcheck source=hack/e2e-lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)/e2e-lib.sh"

# The standalone repo was archived on 2026-10-12 (the suite moved into ceph/ceph), so
# this last commit never changes.
S3TESTS_SHA="5b3987116eca0b4b8fa57d6b10f3db57bab196b6"
SRC="${ROOT_DIR}/bin/s3-tests"
VENV="${ROOT_DIR}/bin/s3tests-venv"
PASS_LIST="${ROOT_DIR}/hack/s3tests-pass.txt"
REQUIREMENTS="${ROOT_DIR}/hack/s3tests-requirements.txt"

# Whole features plainbucket answers with 501 or does not model (one owner, no IAM).
EXCLUDE=(
    abac_test appendobject auth_aws2 bucket_encryption bucket_logging bucket_logging_cleanup
    bucket_policy cloud_restore cloud_transition delete_marker encryption fails_on_aws
    fails_without_logging_rollover group group_policy iam_account iam_cross_account iam_role
    iam_tenant iam_user lifecycle lifecycle_expiration lifecycle_transition object_lock
    object_ownership role_policy s3control s3select s3website s3website_redirect_location
    s3website_routing_rules session_policy sns sse_s3 storage_class tagging target_by_bucket
    test_of_sts token_claims_trust_policy_test token_principal_tag_role_policy_test
    token_request_tag_trust_policy_test token_resource_tags_test token_role_tags_test
    token_tag_keys_test user_policy versioning webidentity_test
)
EXPR=""
for m in "${EXCLUDE[@]}"; do EXPR="${EXPR:+${EXPR} and }not ${m}"; done

if [ "$(git -C "${SRC}" rev-parse HEAD 2>/dev/null)" != "${S3TESTS_SHA}" ]; then
    rm -rf "${SRC:?}"
    git init -q "${SRC}"
    git -C "${SRC}" fetch -q --depth 1 https://github.com/ceph/s3-tests.git "${S3TESTS_SHA}"
    git -C "${SRC}" checkout -q FETCH_HEAD
fi

# The venv keeps a copy of the requirements, written last, so a changed file or a failed
# install rebuilds it. S3TESTS_PYTHON names the interpreter (CI passes setup-python's).
if [ ! -x "${VENV}/bin/pytest" ] || ! cmp -s "${REQUIREMENTS}" "${VENV}/requirements.txt"; then
    rm -rf "${VENV:?}"
    if [ -z "${S3TESTS_PYTHON:-}" ] && command -v uv >/dev/null 2>&1; then
        uv venv -q --python 3.12 "${VENV}"
        uv pip install -q --python "${VENV}/bin/python" -r "${REQUIREMENTS}"
    else
        "${S3TESTS_PYTHON:-python3}" -m venv "${VENV}"
        "${VENV}/bin/pip" install -q -r "${REQUIREMENTS}"
    fi
    cp "${REQUIREMENTS}" "${VENV}/requirements.txt"
fi

# Every key but main is unscoped: s3-tests tears down its buckets with main and alt.
cat >"${WORK}/creds.yaml" <<'EOF_CREDS'
- {access-key: s3tests-alt, secret-key: s3tests-alt-secret}
- {access-key: s3tests-tenant, secret-key: s3tests-tenant-secret}
- {access-key: s3tests-iam, secret-key: s3tests-iam-secret}
EOF_CREDS
# main's owner matches the fixed owner plainbucket reports.
cat >"${WORK}/s3tests.conf" <<EOF_CONF
[DEFAULT]
host = 127.0.0.1
port = ${PORT}
is_secure = False
ssl_verify = False

[fixtures]
bucket prefix = s3test-{random}-
iam name prefix = s3-tests-
iam path prefix = /s3-tests/

[s3 main]
display_name = plainbucket
user_id = plainbucket
email = main@example.com
api_name = default
access_key = ${E2E_ACCESS_KEY}
secret_key = ${E2E_SECRET_KEY}

[s3 alt]
display_name = alt
user_id = alt
email = alt@example.com
access_key = s3tests-alt
secret_key = s3tests-alt-secret

[s3 tenant]
display_name = tenant
user_id = tenant
email = tenant@example.com
access_key = s3tests-tenant
secret_key = s3tests-tenant-secret
tenant = tenant

[iam]
display_name = iam
user_id = iam
email = iam@example.com
access_key = s3tests-iam
secret_key = s3tests-iam-secret

[iam root]
access_key = s3tests-iam
secret_key = s3tests-iam-secret
account_id = iam
user_id = iam
email = iam@example.com

[iam alt root]
access_key = s3tests-alt
secret_key = s3tests-alt-secret
account_id = alt
user_id = alt
email = alt@example.com
EOF_CONF

PLAINBUCKET_CREDENTIALS_FILE="${WORK}/creds.yaml" start_server

echo "s3-tests ${S3TESTS_SHA:0:12}: running"
# Exit code 1 (some tests failed) is the norm and the pass list decides; any other
# nonzero code means pytest itself broke, and the report cannot be trusted.
rc=0
(cd "${SRC}" && S3TEST_CONF="${WORK}/s3tests.conf" AWS_MAX_ATTEMPTS=1 AWS_RETRY_MODE=standard \
    "${VENV}/bin/pytest" -q -p no:cacheprovider -m "${EXPR}" --junitxml="${WORK}/s3tests.xml" \
    s3tests/functional/test_s3.py s3tests/functional/test_headers.py >"${WORK}/pytest.log" 2>&1) || rc=$?
tail -n 1 "${WORK}/pytest.log"
cp "${WORK}/pytest.log" "${ROOT_DIR}/bin/s3tests.log"
if [ "${rc}" -gt 1 ] || [ ! -s "${WORK}/s3tests.xml" ]; then
    echo "pytest exited ${rc}; see bin/s3tests.log" >&2
    tail -n 20 "${WORK}/pytest.log" >&2
    exit 1
fi
if ! kill -0 "${SERVER_PID}" 2>/dev/null; then
    echo "plainbucket exited during the run" >&2
    cat "${WORK}/server.log" >&2
    exit 1
fi
cp "${WORK}/s3tests.xml" "${ROOT_DIR}/bin/s3tests.xml"

python3 - "${WORK}/s3tests.xml" "${PASS_LIST}" "${S3TESTS_UPDATE:-}" <<'EOF_PY'
import sys
import xml.etree.ElementTree as ET

xml_path, list_path, update = sys.argv[1:4]
passed = set()
for case in ET.parse(xml_path).iter("testcase"):
    if not any(c.tag in ("failure", "error", "skipped") for c in case):
        passed.add(case.get("classname").rsplit(".", 1)[-1] + "::" + case.get("name"))

if update:
    if not passed:
        sys.exit("no test passed; refusing to write an empty pass list")
    with open(list_path, "w") as f:
        f.writelines(t + "\n" for t in sorted(passed))
    print(f"wrote {len(passed)} passing tests to {list_path}")
    sys.exit(0)

with open(list_path) as f:
    expected = {line.strip() for line in f if line.strip() and not line.startswith("#")}
if not expected:
    sys.exit(f"{list_path} lists no tests; run make s3tests-update and commit it")
lost = sorted(expected - passed)
new = sorted(passed - expected)
for t in new:
    print(f"  new pass (add to the list): {t}")
for t in lost:
    print(f"  FAIL no longer passes: {t}", file=sys.stderr)
print(f"{len(expected) - len(lost)}/{len(expected)} listed tests pass, {len(new)} new")
sys.exit(1 if lost else 0)
EOF_PY
