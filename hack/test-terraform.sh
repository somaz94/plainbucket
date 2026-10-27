#!/usr/bin/env bash
# End-to-end check with the official terraform binary: its s3 backend keeps state in
# plainbucket and locks it with a lock file (use_lockfile, written with If-None-Match),
# across applies, a lock held by someone else, force-unlock, workspaces and destroy.
#
# Usage: hack/test-terraform.sh   (after `make build`; needs curl and unzip)
# The verified release zip is kept in bin/ so later runs skip the download.
set -euo pipefail

TF_VERSION="1.17.0"

# shellcheck source=hack/e2e-lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)/e2e-lib.sh"

read -r os arch <<<"$(detect_platform)"
# From https://releases.hashicorp.com/terraform/${TF_VERSION}/terraform_${TF_VERSION}_SHA256SUMS
case "${os}-${arch}" in
    linux-amd64) want=c168cab7c637a8769fdb90089ddd19de701bc6ff8aca64dc2e2185fe46f59f67 ;;
    linux-arm64) want=f32478134ffc6d710eec474bbd316652cdddc0f4e336973a3745e8d3a3309cdf ;;
    darwin-amd64) want=3773397524196e4ebbf7167e2b9fb478dff1962d555d649f616ba97ca446a143 ;;
    darwin-arm64) want=7472a54cca911d46c0e399a5ebbce44444912d0d4dad7495f37f1fa51e7ca6b7 ;;
    *) echo "no pinned terraform checksum for ${os}-${arch}" >&2; exit 1 ;;
esac

ZIP="${ROOT_DIR}/bin/terraform_${TF_VERSION}_${os}_${arch}.zip"
fetch_pinned "${ZIP}" "${want}" \
    "https://releases.hashicorp.com/terraform/${TF_VERSION}/terraform_${TF_VERSION}_${os}_${arch}.zip" 0644 terraform
unzip -q -o "${ZIP}" terraform -d "${WORK}/bin"
TF="${WORK}/bin/terraform"

# The developer's TF_* and AWS_* settings (AWS_PROFILE, AWS_SESSION_TOKEN, TF_WORKSPACE, TF_DATA_DIR, ...) would change what runs.
for v in $(env | sed -nE 's/^((TF|AWS)_[A-Za-z0-9_]*)=.*/\1/p'); do unset "${v}"; done
export AWS_ACCESS_KEY_ID="${E2E_ACCESS_KEY}"
export AWS_SECRET_ACCESS_KEY="${E2E_SECRET_KEY}"
export AWS_EC2_METADATA_DISABLED=true
export AWS_CONFIG_FILE=/dev/null
export AWS_SHARED_CREDENTIALS_FILE=/dev/null
# Keep the developer's ~/.terraformrc (plugin cache, mirrors) out of the run.
: >"${WORK}/terraformrc"
export TF_CLI_CONFIG_FILE="${WORK}/terraformrc"
export TF_IN_AUTOMATION=1 TF_INPUT=0 CHECKPOINT_DISABLE=1

start_server
echo "terraform ${TF_VERSION} s3 backend e2e against ${ENDPOINT}"

s3() { curl -fsS --aws-sigv4 "aws:amz:us-east-1:s3" --user "${E2E_ACCESS_KEY}:${E2E_SECRET_KEY}" "$@"; }
s3 -X PUT "${ENDPOINT}/tf-state" >/dev/null || fail "create the state bucket"

mkdir -p "${WORK}/tf"
cat >"${WORK}/tf/main.tf" <<EOF_TF
terraform {
  backend "s3" {
    bucket                      = "tf-state"
    key                         = "app/terraform.tfstate"
    region                      = "us-east-1"
    endpoints                   = { s3 = "${ENDPOINT}" }
    use_path_style              = true
    use_lockfile                = true
    skip_credentials_validation = true
    skip_requesting_account_id  = true
    skip_metadata_api_check     = true
    skip_region_validation      = true
  }
}
variable "input" { default = "one" }
resource "terraform_data" "x" { input = var.input }
EOF_TF
tf() { "${TF}" -chdir="${WORK}/tf" "$@" -no-color >"${WORK}/tf.log" 2>&1; }
tf_log() { cat "${WORK}/tf.log" >&2; }

STATE="${DATA}/tf-state/app/terraform.tfstate"
LOCK="${STATE}.tflock"

tf init || { tf_log; fail "init"; }
ok "init with the s3 backend"
tf apply -auto-approve || { tf_log; fail "apply"; }
grep -q '"input"' "${STATE}" 2>/dev/null && grep -q '"one"' "${STATE}" && ok "state is a plain file" || fail "state on disk"
[ ! -e "${LOCK}" ] && ok "lock file removed after apply" || fail "lock file left"
tf plan -detailed-exitcode && ok "plan from the stored state shows no changes" || { tf_log; fail "plan after apply"; }
tf apply -auto-approve -var input=two && grep -q '"two"' "${STATE}" && ok "apply replaces the state" || { tf_log; fail "second apply"; }

printf '{"ID":"held-elsewhere","Operation":"OperationTypeApply","Who":"e2e"}' >"${WORK}/lock.json"
s3 -H 'If-None-Match: *' -T "${WORK}/lock.json" "${ENDPOINT}/tf-state/app/terraform.tfstate.tflock" >/dev/null || fail "hold the lock"
[ -f "${LOCK}" ] || fail "lock file on disk"
if tf plan -lock-timeout=0s; then tf_log; fail "plan ran under a held lock"; fi
grep -q "Error acquiring the state lock" "${WORK}/tf.log" && grep -q "held-elsewhere" "${WORK}/tf.log" &&
    ok "a held lock stops a plan" || { tf_log; fail "lock error"; }
tf force-unlock -force held-elsewhere && [ ! -e "${LOCK}" ] && ok "force-unlock removes the lock file" || { tf_log; fail "force-unlock"; }

tf workspace new staging && tf apply -auto-approve -var input=staging || { tf_log; fail "workspace apply"; }
grep -q '"staging"' "${DATA}/tf-state/env:/staging/app/terraform.tfstate" && ok "workspace state under env:/" || fail "workspace state on disk"
tf workspace list && grep -q "staging" "${WORK}/tf.log" && ok "workspace list" || { tf_log; fail "workspace list"; }
tf destroy -auto-approve -var input=staging && tf workspace select default && tf workspace delete staging &&
    [ ! -e "${DATA}/tf-state/env:/staging/app/terraform.tfstate" ] && ok "workspace destroyed and deleted" || { tf_log; fail "workspace delete"; }

tf destroy -auto-approve -var input=two || { tf_log; fail "destroy"; }
[ -f "${STATE}" ] || fail "state file gone after destroy"
if grep -q '"terraform_data"' "${STATE}"; then fail "state after destroy"; fi
ok "destroy empties the state"

echo "all ${PASS} checks passed"
