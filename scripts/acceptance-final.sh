#!/usr/bin/env bash
# SKYBRIDGE local production-readiness acceptance (v1).
#
# Read-only + local test execution ONLY. This script MUST NEVER run:
#   terraform apply / terraform destroy / AWS or Azure API mutation /
#   production credentials / reverse CDC / automatic post-authority rollback.
# It runs Go builds, unit/race/contract tests, and Terraform fmt+validate
# (./scripts/tf-plan.sh never applies). Terraform resolution honors
# TERRAFORM_BIN with PATH fallback (see scripts/tf-plan.sh); e.g.:
#   TERRAFORM_BIN=/tmp/tfbin/terraform ./scripts/acceptance-final.sh
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LOGDIR="${TMPDIR:-/tmp}/skybridge-acceptance-$$"
mkdir -p "$LOGDIR"
OVERALL=0

# magnet <NAME> <cmd...>: the check passes only on exit 0 AND >=1 PASS line.
# (go test -run with zero matches exits 0 vacuously, so the count guard is
# mandatory; "no tests ran" always fails.)
magnet() {
  local name="$1"; shift
  local log="$LOGDIR/$name.log"
  if "$@" >"$log" 2>&1; then
    local passes
    passes=$(grep -cE '^(--- PASS|ok |PASS|ALL_VALIDATE_OK|FMT_CLEAN)' "$log" || true)
    if [ "$passes" -ge 1 ] && ! grep -q "no tests ran" "$log"; then
      echo "${name}_OK"
      return 0
    fi
  fi
  echo "${name}_FAIL (see $log)"
  OVERALL=1
  return 0
}

# gom <NAME> <moduledir> <go test args...>: go test inside a module dir
# ("$@" preserves alternation patterns as single words).
gom() {
  local name="$1"; local dir="$2"; shift 2
  magnet "$name" bash -c 'cd "$0" && go test -count=1 -v "$@"' "$dir" "$@"
}

CP="$ROOT/apps/control-plane"
CDC="$ROOT/apps/cdc-applier"
SHOP="$ROOT/workloads/cloudshop/api"

magnet ARCHITECTURE bash -c "cd '$CP' && go build ./... && cd '$CDC' && go build ./... && cd '$SHOP' && go build ./... && echo PASS builds"
magnet CONTRACTS bash -c "cd '$ROOT' && python3 scripts/validate_contracts.py --all && python3 -m pytest tests/contract -q && echo PASS contracts"
gom IDEMPOTENCY "$CP" -run 'TestIdem|TestRegisterReplay|TestIdempotency'
gom COMPATIBILITY "$CP" -run 'TestCompat|TestRegistry'
gom PLANNING "$CP" -run 'TestPlan'
gom DRIFT "$CP" -run 'TestDrift'
gom POLICY "$CP" -run 'TestPolicy'
gom APPROVAL "$CP" -run 'TestApproval'
gom TEMPORAL "$CP" -run 'TestWorkflow|TestActivit|TestExecution'
gom ADAPTER "$CP" -run 'TestAdapter|TestMockProvision'
magnet TERRAFORM_VALIDATE bash -c "cd '$ROOT' && ./scripts/tf-plan.sh"
gom CDC "$CDC" -race ./...
gom REHEARSAL "$CP" -run 'TestRehearsal'
gom CANARY "$CP" -run 'TestCanary'
gom QUIESCE "$SHOP" -race -run 'Quiesce|WRITES_PAUSED|WritesPaused'
gom OWNERSHIP "$SHOP" -race -run 'Ownership|SplitBrain|PostCutover'
gom CUTOVER "$CP" -race -run 'TestCutover'
gom RECOVERY "$CP" -run 'TestCutoverQuiesceTransitionFailure|TestCutoverCDCFailure|TestCutoverVerifyFailure|TestCutoverWorkerRestart|TestCutoverResumeFailure|TestCutoverTargetFlipFailure|TestCutoverRollbackBlocked|TestCutoverPreTransferRollback|TestCutoverStaleWorker|TestCutoverConcurrency|TestCutoverReplay'
gom SPLIT_BRAIN "$SHOP" -race -run 'TestSplitBrain|TestOwnershipEnforcement'
gom OBSERVABILITY "$CP" -run 'TestCutoverStatus|TestCutoverCounters|TestObservability|TestCutoverStateMachineAudit'

if [ "$OVERALL" -eq 0 ]; then
  echo "SKYBRIDGE_LOCAL_PRODUCTION_READINESS = PASS"
else
  echo "SKYBRIDGE_LOCAL_PRODUCTION_READINESS = FAIL (logs in $LOGDIR)"
fi
exit "$OVERALL"
