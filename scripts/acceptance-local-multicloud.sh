#!/usr/bin/env bash
# SKYBRIDGE free local multi-cloud acceptance: full demo + failure matrix +
# reset, with zero cloud calls and zero spend.
#
# Flow: prerequisites -> reset -> demo-migration.sh -> live/unit magnets ->
# demo-failures.sh -> reset -> reset + safety magnets -> verdict.
# Every magnet requires exit 0 AND >=1 PASS line (defeats vacuous runs).
# Env: CP_PORT/SRC_PORT/TGT_PORT (18080/8085/8086), DEMO_RUN_ID.
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export CP_PORT="${CP_PORT:-18080}" SRC_PORT="${SRC_PORT:-8085}" TGT_PORT="${TGT_PORT:-8086}"
export CONTROL_PLANE_URL="http://localhost:$CP_PORT"
export SHOP_SRC_URL="http://localhost:$SRC_PORT"
export SHOP_TGT_URL="http://localhost:$TGT_PORT"
export DEMO_RUN_ID="${DEMO_RUN_ID:-$(date +%s)}"
export COMPOSE="${COMPOSE:-docker compose}"
export ROOT
export DEMO_CUTOVER_JSON="$ROOT/.demo-run/cutover.json"
LOGDIR="${TMPDIR:-/tmp}/skybridge-acceptance-demo-$$"
mkdir -p "$LOGDIR"
OVERALL=0

magnet() {
  local name="$1"; shift
  local log="$LOGDIR/$name.log"
  if "$@" >"$log" 2>&1; then
    local passes
    passes=$(grep -cE '^(--- PASS|ok |PASS|RESET_OK|DEMO_MIGRATION|FAILURE_MATRIX)' "$log" || true)
    if [ "$passes" -ge 1 ] && ! grep -q "no tests ran" "$log"; then
      echo "${name}_OK"
      return 0
    fi
  fi
  echo "${name}_FAIL (see $log)"
  OVERALL=1
  return 0
}

gom() {
  local name="$1"; local dir="$2"; shift 2
  magnet "$name" bash -c 'cd "$0" && go test -count=1 -v "$@"' "$dir" "$@"
}

CP="$ROOT/apps/control-plane"
CDC="$ROOT/apps/cdc-applier"
SHOP="$ROOT/workloads/cloudshop/api"

magnet LOCAL_PREREQUISITES bash -c "
  command -v go && command -v python3 && command -v curl && command -v docker &&
  docker compose -f '$ROOT/docker-compose.yml' ps cloudshop-db 2>/dev/null | grep -q . &&
  curl -s -m 8 http://localhost:8083/connectors >/dev/null &&
  curl -s -m 8 http://localhost:9644/v1/status/ready | grep -q ready &&
  echo PREREQ PASS &&
  echo PASS"
"$ROOT/scripts/reset-demo.sh" >/dev/null 2>&1
"$ROOT/scripts/demo-migration.sh" >"$LOGDIR/demo.log" 2>&1 || { echo "demo-migration.sh failed (see $LOGDIR/demo.log)"; OVERALL=1; }
grep -q "DEMO_MIGRATION = COMPLETE" "$LOGDIR/demo.log" 2>/dev/null || OVERALL=1

magnet SOURCE_ENV bash -c "
  curl -s -m 8 '$SHOP_SRC_URL/readyz' | grep -q '\"ready\":true' &&
  curl -s -m 8 '$SHOP_SRC_URL/v1/admin/quiesce' | grep -q 'write_ownership' &&
  echo PASS source"
magnet TARGET_ENV bash -c "
  curl -s -m 8 '$SHOP_TGT_URL/readyz' | grep -q '\"ready\":true' &&
  echo PASS target"
gom CLOUDSHOP "$SHOP" ./...
gom CDC "$CDC" ./...
gom COMPATIBILITY "$CP" -run 'TestCompat|TestRegistry'
gom PLAN "$CP" -run 'TestPlan'
gom DRIFT "$CP" -run 'TestDrift'
gom POLICY "$CP" -run 'TestPolicy'
gom APPROVAL "$CP" -run 'TestApproval'
gom REHEARSAL "$CP" -run 'TestRehearsal'
gom CANARY "$CP" -run 'TestCanary'
gom QUIESCE "$CP" -run 'TestQuiesce'
gom OWNERSHIP "$SHOP" -run 'Ownership|SplitBrain|PostCutover'
gom CUTOVER "$CP" -run 'TestCutover'
magnet AZURE_WRITE bash -c '
  TGT_USER=$($COMPOSE -f "$ROOT/docker-compose.yml" exec -T cloudshop-target-db psql -U cloudshop -d cloudshop -t -c "SELECT id FROM users LIMIT 1;" 2>/dev/null | tr -d " \n\r\t") &&
  [ -n "$TGT_USER" ] &&
  curl -s -m 10 -X POST "$SHOP_TGT_URL/v1/orders" -H "Idempotency-Key: acc-azure-$DEMO_RUN_ID" \
    -d "{\"user_id\":\"$TGT_USER\",\"total_cents\":6161}" | python3 -c "import sys,json; assert json.load(sys.stdin)[\"status\"]==\"pending\"" 2>/dev/null &&
  echo PASS azure-write'
magnet AWS_WRITE_REJECTED bash -c '
  [ "$(curl -s -m 10 -o /dev/null -w "%{http_code}" -X POST "$SHOP_SRC_URL/v1/orders" -H "Idempotency-Key: acc-aws-$DEMO_RUN_ID" \
    -d "{\"user_id\":\"91653b05-e7bd-42c4-8b62-af09019f07a6\",\"total_cents\":6162}")" = "403" ] &&
  echo PASS aws-rejected'
magnet SPLIT_BRAIN bash -c '
  src=$(curl -s -m 8 "$SHOP_SRC_URL/v1/admin/ownership" | python3 -c "import sys,json; print(json.load(sys.stdin)[\"write_ownership\"])") &&
  tgt=$(curl -s -m 8 "$SHOP_TGT_URL/v1/admin/ownership" | python3 -c "import sys,json; print(json.load(sys.stdin)[\"write_ownership\"])") &&
  [ "$src" = "azure" ] && [ "$tgt" = "azure" ] &&
  echo PASS split-brain'
magnet RECONCILIATION bash -c '
  python3 -c "import json, os; d=json.load(open(os.environ.get(\"DEMO_CUTOVER_JSON\"))); assert d[\"status\"]==\"CUTOVER_COMPLETE\" and d[\"reconciliation\"][\"match\"] is True" &&
  echo PASS reconciliation'

"$ROOT/scripts/demo-failures.sh" >"$LOGDIR/failures.log" 2>&1
if grep -q "RESULT=FAIL" "$LOGDIR/failures.log" || ! grep -q "FAILURE_MATRIX = COMPLETE" "$LOGDIR/failures.log"; then
  echo "FAILURE_MATRIX_FAIL (see $LOGDIR/failures.log)"; OVERALL=1
else
  echo "FAILURE_MATRIX_OK"
fi

"$ROOT/scripts/reset-demo.sh" >"$LOGDIR/reset.log" 2>&1
magnet RESET bash -c "
  grep -q RESET_OK '$LOGDIR/reset.log' &&
  ! curl -s -m 3 '$CONTROL_PLANE_URL/healthz' >/dev/null 2>&1 &&
  ! curl -s -m 3 '$SHOP_SRC_URL/healthz' >/dev/null 2>&1 &&
  ! curl -s -m 3 '$SHOP_TGT_URL/healthz' >/dev/null 2>&1 &&
  echo PASS reset"
magnet NO_CLOUD_CALLS bash -c "
  cd '$ROOT' &&
  ! grep -rn 'aws-sdk-go\|azure-sdk\|management.azure.com\|sts.amazonaws.com' apps/ workloads/ --include='*.go' | grep -v _test | grep -q . &&
  ! grep -rnE 'terraform (apply|destroy)' scripts/*.sh | grep -v 'NEVER\|never runs\|MUST NEVER' | grep -vE ':[0-9]+:[[:space:]]*#' | grep -q . &&
  grep -q 'ApplyEnabled = false' apps/control-plane/tfvars.go &&
  echo PASS no-cloud"
magnet NO_SECRETS bash -c "cd '$CP' && go test -count=1 -v -run 'TestNoStaticCredentials|TestAdapterNoSecrets|TestObservabilityNoSecrets' ./..."

if [ "$OVERALL" -eq 0 ]; then
  echo "SKYBRIDGE_FREE_LOCAL_MULTICLOUD_DEMO = PASS"
else
  echo "SKYBRIDGE_FREE_LOCAL_MULTICLOUD_DEMO = FAIL (logs in $LOGDIR)"
fi
exit "$OVERALL"
