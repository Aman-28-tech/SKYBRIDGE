#!/usr/bin/env bash
# SKYBRIDGE showcase: the full live demonstration in one command.
#
#   1. doctor            (lab health, read-only)
#   2. reset             (AWS authoritative, Azure standby)
#   3. demo-migration    (full AWS->Azure lifecycle, CUTOVER_COMPLETE)
#   4. demo-failures     (scenarios A-L, FAILURE_MATRIX = COMPLETE)
#   5. reset             (clean deterministic state)
#   6. status            (current-state summary)
#
# Local infrastructure only. Zero AWS calls, zero Azure calls, $0 spend:
# a read-only safety gate asserts the cloud boundary before the run.
# Final line: SKYBRIDGE_SHOWCASE = PASS. Exit code matches the verdict.
#
# Env (all optional): DEMO_RUN_ID (shared deterministic tag; export the
# same value to repeat an identical setup), CP_PORT/SRC_PORT/TGT_PORT,
# COMPOSE.
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export DEMO_RUN_ID="${DEMO_RUN_ID:-$(date +%s)}"
export CP_PORT="${CP_PORT:-18080}" SRC_PORT="${SRC_PORT:-8085}" TGT_PORT="${TGT_PORT:-8086}"
export CONTROL_PLANE_URL="http://localhost:$CP_PORT"
export SHOP_SRC_URL="http://localhost:$SRC_PORT"
export SHOP_TGT_URL="http://localhost:$TGT_PORT"
export COMPOSE="${COMPOSE:-docker compose}"
OVERALL=0
echo "SHOWCASE_RUN_ID=$DEMO_RUN_ID"

step() { # name, command...
  local name="$1"; shift
  echo "--- showcase: $name ---"
  if "$@"; then echo "SHOWCASE_STEP $name = PASS"; else echo "SHOWCASE_STEP $name = FAIL"; OVERALL=1; fi
}

gate() { # read-only local-only boundary check (mirrors acceptance NO_CLOUD_CALLS)
  cd "$ROOT" || return 1
  ! grep -rn 'aws-sdk-go\|azure-sdk\|management.azure.com\|sts.amazonaws.com' apps/ workloads/ --include='*.go' | grep -v _test | grep -q . || return 1
  ! grep -rnE 'terraform (apply|destroy)' scripts/*.sh | grep -v 'NEVER\|never runs\|MUST NEVER' | grep -vE ':[0-9]+:[[:space:]]*#' | grep -q . || return 1
  grep -q 'ApplyEnabled = false' apps/control-plane/tfvars.go || return 1
  echo "LOCAL_ONLY_BOUNDARY_OK (no cloud SDKs, no apply/destroy, ApplyEnabled=false)"
}

step CLOUD_GATE gate
if [ "$OVERALL" -ne 0 ]; then echo "SKYBRIDGE_SHOWCASE = FAIL"; exit 1; fi
step DOCTOR "$ROOT/scripts/doctor.sh"
step RESET_BEFORE "$ROOT/scripts/reset-demo.sh"
step DEMO_MIGRATION "$ROOT/scripts/demo-migration.sh"
step DEMO_FAILURES "$ROOT/scripts/demo-failures.sh"
step RESET_AFTER "$ROOT/scripts/reset-demo.sh"
step STATUS "$ROOT/scripts/skybridge-status.sh"

if [ "$OVERALL" -eq 0 ]; then
  echo "SKYBRIDGE_SHOWCASE = PASS"
else
  echo "SKYBRIDGE_SHOWCASE = FAIL"
fi
exit "$OVERALL"
