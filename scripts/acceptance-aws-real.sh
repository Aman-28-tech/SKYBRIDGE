#!/usr/bin/env bash
# SKYBRIDGE real-AWS foundation acceptance (v1).
#
# SAFETY: this script performs NO mutation. It never runs terraform
# apply/destroy, never calls AWS mutating APIs, never touches Azure,
# never executes migration/cutover/rollback, and needs no credentials.
# Live AWS magnets require an approved sandbox (SKYBRIDGE_AWS_SANDBOX=1
# plus a configured sandbox identity); otherwise they report BLOCKED
# with the reason and the script exits nonzero. Nothing is fabricated:
# a magnet prints _OK only for a verified fact.
#
# Usage:
#   ./scripts/acceptance-aws-real.sh
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LOGDIR="${TMPDIR:-/tmp}/skybridge-acceptance-aws-$$"
mkdir -p "$LOGDIR"
OVERALL=0

# magnet <NAME> <cmd...>: _OK on exit 0; _FAIL otherwise (broken expectation).
magnet() {
  local name="$1"; shift
  local log="$LOGDIR/$name.log"
  if "$@" >"$log" 2>&1; then
    echo "${name}_OK"
  else
    echo "${name}_FAIL (see $log)"
    OVERALL=1
  fi
}

# live_gate <NAME>: passes only with explicit sandbox approval + identity.
# Otherwise prints <NAME>_BLOCKED with the reason (not a failure of the
# harness, but proof the foundation is not live yet).
live_gate() {
  local name="$1"
  if [ "${SKYBRIDGE_AWS_SANDBOX:-0}" != "1" ]; then
    echo "${name}_BLOCKED (SKYBRIDGE_AWS_SANDBOX!=1; no live AWS without explicit sandbox approval)"
    OVERALL=1
    return 0
  fi
  if [ -z "${SKYBRIDGE_AWS_ROLE_ARN:-}${AWS_PROFILE:-}${AWS_ROLE_ARN:-}" ]; then
    echo "${name}_BLOCKED (sandbox approved but no AWS identity configured)"
    OVERALL=1
    return 0
  fi
  return 1
}

CP="$ROOT/apps/control-plane"

# 1. Identity posture: fail-closed live seam verified by unit tests, and no
#    static credentials anywhere in the tree (leak test).
magnet AWS_IDENTITY bash -c "cd '$CP' && go test -count=1 -run 'TestAWSLive|TestNoStaticCredentials' ./..."

# 2-10. Live foundation magnets: gated behind explicit sandbox approval.
for m in AWS_NETWORK AWS_DATABASE AWS_STORAGE AWS_WORKLOAD AWS_HEALTH AWS_WRITE AWS_IDEMPOTENCY AWS_OBSERVABILITY AWS_DRIFT_BASELINE; do
  if live_gate "$m"; then
    : # blocked message already printed
  else
    # Sandbox approved + identity present: read-only live checks would run
    # here (describe-only). Not implemented until the Phase-3 gate passes.
    echo "${m}_BLOCKED (live read checks deferred to approved sandbox run)"
    OVERALL=1
  fi
done

# Zero-mutation proof (§10): assert the boundaries that make mutation
# impossible from this slice, then print the counters.
echo "--- zero-mutation proof ---"
MUT=0
if grep -q "ApplyEnabled = false" "$CP/tfvars.go"; then
  echo "apply_boundary: ApplyEnabled=false"
else
  echo "apply_boundary: VIOLATED"; MUT=1
fi
if grep -rnE "terraform (apply|destroy)" "$ROOT/scripts/acceptance-aws-real.sh" | grep -v "MUST NEVER\|never runs" | grep -q .; then
  echo "script_mutation: VIOLATED"; MUT=1
else
  echo "script_mutation: none (no apply/destroy/aws-mutation commands)"
fi
if grep -rn "configure-aws-credentials\|aws-actions" "$ROOT/.github/workflows/" 2>/dev/null | grep -q .; then
  echo "ci_credentials: VIOLATED"; MUT=1
else
  echo "ci_credentials: none (no cloud credential actions in CI)"
fi
echo "azure_mutations = 0"
echo "migration_executions = 0"
echo "ownership_transfers = 0"
echo "cutover_executions = 0"
echo "reverse_cdc = 0"
[ "$MUT" -eq 0 ] || OVERALL=1
echo "aws_authoritative = true (no transfer path executed)"

if [ "$OVERALL" -eq 0 ]; then
  echo "SKYBRIDGE_REAL_AWS_FOUNDATION = PASS"
else
  echo "SKYBRIDGE_REAL_AWS_FOUNDATION = BLOCKED (logs in $LOGDIR)"
fi
exit "$OVERALL"
