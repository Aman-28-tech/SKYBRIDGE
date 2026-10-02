#!/usr/bin/env bash
# Safe Terraform check: fmt + validate every module and root.
# `terraform plan` is deliberately NOT run here: it requires provider
# credentials, which exist only in CI with OIDC (see .github/workflows/terraform.yml
# and docs/TERRAFORM_IAC.md). No apply, no destroy, ever from this script.
set -euo pipefail
# Binary resolution: explicit TERRAFORM_BIN wins, then legacy TF, then PATH.
# Fails clearly when none exists. Never hardcodes a local install path.
TF_BIN="${TERRAFORM_BIN:-${TF:-terraform}}"
if ! command -v "$TF_BIN" >/dev/null 2>&1; then
  echo "error: terraform executable not found: '$TF_BIN'" >&2
  echo "hint: install Terraform 1.9.8, add it to PATH, or set TERRAFORM_BIN=/path/to/terraform" >&2
  exit 127
fi
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
"$TF_BIN" fmt -check -recursive "$ROOT/infrastructure/terraform" && echo "FMT_CLEAN"
for d in "$ROOT"/infrastructure/terraform/modules/*/ "$ROOT"/infrastructure/terraform/aws "$ROOT"/infrastructure/terraform/azure; do
  "$TF_BIN" -chdir="$d" init -backend=false -input=false > /dev/null
  "$TF_BIN" -chdir="$d" validate
  rm -rf "$d/.terraform" "$d/.terraform.lock.hcl"
done
echo "ALL_VALIDATE_OK"
echo "NOTE: terraform plan/apply require OIDC provider credentials (CI protected environments only)."
