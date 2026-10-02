#!/usr/bin/env bash
# SKYBRIDGE test-all: runs every applicable suite once and prints exact
# counts. Unit/race suites, contract tests, validator, compose validation,
# Terraform fmt/validate, then the full local acceptance (reset + demo +
# unit magnets + failures + reset + safety magnets).
#
# Env (all optional): DEMO_RUN_ID (forwarded to local acceptance),
# COMPOSE, TERRAFORM_BIN/TF (terraform resolution, see tf-plan.sh).
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export DEMO_RUN_ID="${DEMO_RUN_ID:-$(date +%s)}"
export COMPOSE="${COMPOSE:-docker compose}"
LOGDIR="${TMPDIR:-/tmp}/skybridge-test-all-$$"
mkdir -p "$LOGDIR"
OVERALL=0

go_suite() { # name dir [go test args...]
  local name="$1" dir="$2"; shift 2
  local log="$LOGDIR/$name.log"
  if (cd "$dir" && go test -count=1 -v "$@" >"$log" 2>&1); then
    local rc=0
  else
    local rc=1
  fi
  local passed failed
  passed=$(grep -cE '^--- PASS' "$log" || true)
  failed=$(grep -cE '^--- FAIL' "$log" || true)
  echo "$name: passed=$passed failed=$failed (go test $* in $dir)"
  if [ "$rc" -ne 0 ] || [ "$failed" -ne 0 ] || [ "$passed" -eq 0 ]; then OVERALL=1; return 1; fi
}

CP="$ROOT/apps/control-plane"
CDC="$ROOT/apps/cdc-applier"
SHOP="$ROOT/workloads/cloudshop/api"

go_suite CONTROL_PLANE_UNIT "$CP" ./...
go_suite CONTROL_PLANE_RACE "$CP" -race ./...
go_suite CDC_APPLIER "$CDC" ./...
go_suite CLOUDSHOP "$SHOP" ./...

PYLOG="$LOGDIR/pytest.log"
if python3 -m pytest "$ROOT/tests/contract" -q >"$PYLOG" 2>&1; then
  PY_SUMMARY=$(tail -n 2 "$PYLOG" | tr '\n' ' ')
else
  PY_SUMMARY="FAILED $(tail -n 2 "$PYLOG" | tr '\n' ' ')"; OVERALL=1
fi
echo "PYTEST_CONTRACT: $PY_SUMMARY"

VALLOG="$LOGDIR/validator.log"
if python3 "$ROOT/scripts/validate_contracts.py" --all >"$VALLOG" 2>&1; then
  echo "VALIDATOR: passed=$(grep -c '^PASS' "$VALLOG" || true) contract checks"
else
  echo "VALIDATOR: FAIL (see $VALLOG)"; OVERALL=1
fi

if $COMPOSE -f "$ROOT/docker-compose.yml" config -q >/dev/null 2>&1; then
  echo "COMPOSE_VALIDATION: PASS (docker compose config clean)"
else
  echo "COMPOSE_VALIDATION: FAIL"; OVERALL=1
fi

TFLOG="$LOGDIR/terraform.log"
# Shared provider plugin cache: terraform natively honors
# TF_PLUGIN_CACHE_DIR, so successfully downloaded providers are reused
# across modules, attempts, and runs instead of re-downloaded from the
# registry every time (the check itself is unchanged: fmt + validate only).
export TF_PLUGIN_CACHE_DIR="$ROOT/.terraform-plugin-cache"
mkdir -p "$TF_PLUGIN_CACHE_DIR" 2>/dev/null || true
# One retry: provider downloads hit the public registry and can flake on a
# constrained network. The check itself is identical on both attempts
# (fmt + validate only, never plan/apply); the log records which attempt
# passed.
TF_ATTEMPT=0; TF_OK=""
while [ "$TF_ATTEMPT" -lt 2 ] && [ -z "$TF_OK" ]; do
  TF_ATTEMPT=$((TF_ATTEMPT+1))
  if "$ROOT/scripts/tf-plan.sh" >"$TFLOG.attempt$TF_ATTEMPT" 2>&1; then TF_OK="$TF_ATTEMPT"; fi
done
cat "$TFLOG.attempt$TF_ATTEMPT" >"$TFLOG" 2>/dev/null || true
if [ -n "$TF_OK" ]; then
  echo "TERRAFORM_FMT_VALIDATE: $(grep -cE 'FMT_CLEAN|ALL_VALIDATE_OK' "$TFLOG") markers, attempt $TF_OK (fmt + validate, no plan/apply)"
else
  echo "TERRAFORM_FMT_VALIDATE: FAIL after $TF_ATTEMPT attempts (see $TFLOG)"; OVERALL=1
fi

ACCLOG="$LOGDIR/acceptance.log"
if "$ROOT/scripts/acceptance-local-multicloud.sh" >"$ACCLOG" 2>&1; then
  echo "LOCAL_ACCEPTANCE: $(grep -cE '_OK$' "$ACCLOG") magnets OK ($(tail -n 1 "$ACCLOG"))"
else
  echo "LOCAL_ACCEPTANCE: FAIL (see $ACCLOG)"; OVERALL=1
fi

if [ "$OVERALL" -eq 0 ]; then
  echo "SKYBRIDGE_TEST_ALL = PASS"
else
  echo "SKYBRIDGE_TEST_ALL = FAIL (logs in $LOGDIR)"
fi
exit "$OVERALL"
