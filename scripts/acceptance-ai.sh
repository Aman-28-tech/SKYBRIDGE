#!/usr/bin/env bash
# SKYBRIDGE AI advisory acceptance (v1).
#
# Verifies the AI planner/validator is advisory-only, evidence-grounded,
# hallucination-guarded, injection-resistant, secret-safe, fail-safe, and
# integrated into the read-only console without any authorization or
# mutation path.
#
# FREE + LOCAL ONLY. This script MUST NEVER: call AWS/Azure, run terraform
# apply/destroy, use cloud credentials, or require paid external APIs. The
# deterministic mock provider serves all model calls; no network beyond
# localhost. Real-cloud gate stays blocked (ApplyEnabled=false untouched).
#
# Env (all optional):
#   CP_PORT (default 18083), AI_PORT (default 18084), CONSOLE_PORT (default 3003)
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CP_PORT="${CP_PORT:-18083}"
AI_PORT="${AI_PORT:-18084}"
CONSOLE_PORT="${CONSOLE_PORT:-3003}"
CONTROL_PLANE_URL="http://localhost:$CP_PORT"
AI_URL="http://localhost:$AI_PORT"
LOGDIR="${TMPDIR:-/tmp}/skybridge-acceptance-ai-$$"
mkdir -p "$LOGDIR"
OVERALL=0
CP_PID=""; AI_PID=""

cleanup() {
  [ -n "$CP_PID" ] && kill "$CP_PID" 2>/dev/null || true
  [ -n "$AI_PID" ] && kill "$AI_PID" 2>/dev/null || true
  if command -v fuser >/dev/null 2>&1; then
    fuser -k "$CP_PORT/tcp" >/dev/null 2>&1 || true
    fuser -k "$AI_PORT/tcp" >/dev/null 2>&1 || true
    fuser -k "$CONSOLE_PORT/tcp" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# magnet <NAME> <cmd...>: prints NAME_OK only on exit 0 AND >=1 PASS line.
magnet() {
  local name="$1"; shift
  local log="$LOGDIR/$name.log"
  if "$@" >"$log" 2>&1; then
    local passes
    passes=$(grep -cE '^[[:space:]]*(--- PASS|ok |PASS|[0-9]+ passed)' "$log" || true)
    if [ "$passes" -ge 1 ] && ! grep -q "no tests ran" "$log"; then
      echo "${name}_OK"
      return 0
    fi
  fi
  echo "${name}_FAIL (see $log)"
  OVERALL=1
  return 0
}

AI_DIR="$ROOT/apps/agent-service"
CONSOLE="$ROOT/apps/console"
CP="$ROOT/apps/control-plane"

# ---- 1. build + unit gates (no backend needed) ----
magnet AI_SERVICE_BUILD bash -c "
  set -o pipefail; cd '$AI_DIR' &&
  python3 -m py_compile skybridge_ai/*.py tests/*.py &&
  python3 -c 'import skybridge_ai.review, skybridge_ai.server; print(\"imports ok\")' &&
  echo PASS ai-service-build"

magnet AI_SCHEMA bash -c "
  set -o pipefail; cd '$AI_DIR' &&
  python3 -m pytest tests/test_hallucination_guard.py -q -k 'invalid_enum or malformed or non_object or missing_field or wrong_authorization or approve_decision or fabricated_policy or migration_id_mismatch' 2>&1 | tail -n 3"

magnet AI_GROUNDING bash -c "
  set -o pipefail; cd '$AI_DIR' &&
  python3 -m pytest tests/test_review_scenarios.py -q -k 'missing_evidence or reproducible or valid_review' 2>&1 | tail -n 3 &&
  python3 -m pytest tests/test_hallucination_guard.py -q -k 'references_must_exist' 2>&1 | tail -n 2"

magnet AI_HALLUCINATION_GUARD bash -c "
  set -o pipefail; cd '$AI_DIR' &&
  python3 -m pytest tests/test_hallucination_guard.py -q 2>&1 | tail -n 3"

magnet AI_PROMPT_INJECTION_GUARD bash -c "
  set -o pipefail; cd '$AI_DIR' &&
  python3 -m pytest tests/test_safety.py -q -k 'injection or hostile or versioned' 2>&1 | tail -n 3"

magnet AI_SECRET_FILTER bash -c "
  set -o pipefail; cd '$AI_DIR' &&
  python3 -m pytest tests/test_safety.py -q -k 'secret or prompt_never' 2>&1 | tail -n 3"

magnet AI_FAILURE_SAFE bash -c "
  set -o pipefail; cd '$AI_DIR' &&
  python3 -m pytest tests/test_safety.py -q -k 'stale or unavailable or timeout or provider_error or invalid_provider or complete_with_timeout' 2>&1 | tail -n 3"

# ---- 2. live advisory review (in-memory control plane + mock provider) ----
cd "$CP" && go build -o "$LOGDIR/control-plane" . || { echo "CONTROL_PLANE_BUILD_FAIL"; OVERALL=1; }
# Local/dev auth (H-1): per-run admin token for the seeded mutations below.
ACC_TOKEN="acc-ai-$(openssl rand -hex 12 2>/dev/null || python3 -c 'import secrets; print(secrets.token_hex(12))')"
ACC_AUTH_HDR="Authorization: Bearer $ACC_TOKEN"
PORT="$CP_PORT" SKYBRIDGE_LOCAL_TOKENS='[{"token":"'"$ACC_TOKEN"'","actor_id":"acc-seed","actor_type":"human","admin":true}]' "$LOGDIR/control-plane" >"$LOGDIR/cp.log" 2>&1 &
CP_PID=$!
for i in $(seq 1 20); do
  curl -s -m 3 "$CONTROL_PLANE_URL/healthz" | grep -q ok && break
  sleep 1
  [ "$i" = 20 ] && { echo "CONTROL_PLANE_START_FAIL (see $LOGDIR/cp.log)"; OVERALL=1; }
done

K="ai-acc-$$"
SPEC='{"schema_version":1,"name":"cloudshop","compute":{"orchestrator":"kubernetes","replicas":2,"cpu_millicores":500,"memory_mib":512},"database":{"engine":"postgresql","major_version":"17","storage_gib":20},"cache":{"engine":"redis","authoritative":false},"object_storage":{"required":true,"versioning_required":true},"queue":{"delivery_semantics":"at_least_once","duplicate_safe_consumer":true},"requirements":{"rpo_seconds":30,"rto_seconds":900,"private_data_plane":true}}'
WID=$(curl -s -m 10 -X POST "$CONTROL_PLANE_URL/v1/workloads" -H "Idempotency-Key: $K-w01" -H "$ACC_AUTH_HDR" -d "$SPEC" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])") || WID=""
MID=$(curl -s -m 10 -X POST "$CONTROL_PLANE_URL/v1/migrations" -H "Idempotency-Key: $K-m01" -H "$ACC_AUTH_HDR" -d '{"workload_id":"'$WID'","target_provider":"azure"}' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])") || MID=""
curl -s -m 10 -X POST "$CONTROL_PLANE_URL/v1/workloads/$WID/compatibility" -H "Idempotency-Key: $K-c01" -H "$ACC_AUTH_HDR" -d '{"target_provider":"azure"}' >"$LOGDIR/compat.json" 2>&1 || true
[ -n "$WID" ] && [ -n "$MID" ] || { echo "SEED_FAIL"; OVERALL=1; }
echo "WORKLOAD=$WID MIGRATION=$MID" >"$LOGDIR/ids.env"

AUDIT_BEFORE=$(curl -s -m 10 "$CONTROL_PLANE_URL/v1/migrations/$MID/audit" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('items',[])))")

cd "$AI_DIR" && SKYBRIDGE_CONTROL_PLANE_URL="$CONTROL_PLANE_URL" SKYBRIDGE_AI_PORT="$AI_PORT" \
  SKYBRIDGE_AI_PROVIDER=mock nohup python3 -m skybridge_ai.server >"$LOGDIR/ai.log" 2>&1 &
AI_PID=$!
for i in $(seq 1 20); do
  curl -s -m 3 "$AI_URL/healthz" | grep -q ok && break
  sleep 1
  [ "$i" = 20 ] && { echo "AI_START_FAIL (see $LOGDIR/ai.log)"; OVERALL=1; }
done

curl -s -m 20 -X POST "$AI_URL/v1/ai/migration-review" -H 'Content-Type: application/json' \
  -d '{"workload_id":"'$WID'","migration_id":"'$MID'","target_provider":"azure"}' >"$LOGDIR/review.json" 2>&1 || true
curl -s -m 20 "$AI_URL/v1/ai/migration-review?migration_id=$MID&target_provider=azure" >"$LOGDIR/review-get.json" 2>&1 || true
curl -s -m 10 -X POST "$AI_URL/v1/ai/migration-review" -H 'Content-Type: application/json' \
  -d '{"workload_id":"'$WID'","migration_id":"00000000-0000-4000-8000-000000000000","target_provider":"azure"}' >"$LOGDIR/review-404.json" 2>&1 || true

check() { # <NAME> <python-expr> <file>
  local name="$1" expr="$2" file="$3"
  local log="$LOGDIR/$name.log"
  if python3 -c "
import json
d = json.load(open('$file'))
assert ($expr)
print('PASS $name')" >"$log" 2>&1; then
    echo "${name}_OK"
  else
    echo "${name}_FAIL (see $log)"
    OVERALL=1
  fi
}

check AI_VALID_REVIEW "d.get('validation_result')=='valid' and d.get('review',{}).get('authorization')=='NEVER_BY_AI' and d.get('prompt_version')=='ai-planner-v1' and d.get('evidence_fingerprint','').startswith('sha256:') and d.get('review',{}).get('confidence') in ('low','medium','high')" "$LOGDIR/review.json"
check AI_VALID_REVIEW_GET "d.get('validation_result')=='valid' and d.get('review',{}).get('authorization')=='NEVER_BY_AI'" "$LOGDIR/review-get.json"
check AI_EVIDENCE_REFERENCE "len(d.get('evidence',[]))>=3 and set(d.get('review',{}).get('assessment',{}).get('evidence_references',[])).issubset({r['id'] for r in d['evidence']}) and all(set(i.get('evidence_refs',[])).issubset({r['id'] for r in d['evidence']}) for i in d['review']['assessment']['risks'] + d['review']['assessment']['warnings'])" "$LOGDIR/review.json"
check AI_FAILSAFE_404 "d.get('review') is None and d.get('validation_result') in ('invalid','failed')" "$LOGDIR/review-404.json"

# Advisory review must not mutate: audit trail length unchanged by POST review.
AUDIT_AFTER=$(curl -s -m 10 "$CONTROL_PLANE_URL/v1/migrations/$MID/audit" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('items',[])))")
if [ "$AUDIT_BEFORE" = "$AUDIT_AFTER" ]; then
  echo "PASS AI_NO_MUTATION audit=$AUDIT_BEFORE unchanged by advisory review" >"$LOGDIR/AI_NO_MUTATION_PATH.log"
  echo "AI_NO_MUTATION_PATH_OK"
else
  echo "AI_NO_MUTATION_PATH_FAIL (audit $AUDIT_BEFORE -> $AUDIT_AFTER)"
  OVERALL=1
fi

# ---- 3. static authorization/mutation-path guards ----
magnet AI_NO_AUTHORIZATION_PATH bash -c "
  set -o pipefail
  cd '$ROOT'
  echo '--- validator forbids authoritative decisions ---'
  grep -q 'FORBIDDEN_DECISION_VALUES' apps/agent-service/skybridge_ai/validate.py &&
  grep -q 'AUTHORIZATION_NEVER' apps/agent-service/skybridge_ai/schema.py &&
  echo '--- no allow/deny/approve/execute decision emission in service ---'
  ! grep -rn --include='*.py' -E '\"decision\"\\s*:\\s*\"(allow|deny|approve|execute)\"' apps/agent-service/skybridge_ai/ &&
  ! grep -rn --include='*.py' -E 'approvalEligibleForUse|decideApproval|TransferOwnership|ApplyInfrastructure|terraform (apply|destroy)' apps/agent-service/skybridge_ai/ &&
  echo PASS no-authorization-path"

magnet AI_NO_MUTATION_STATIC bash -c "
  set -o pipefail
  cd '$ROOT'
  echo '--- control-plane calls are GET-only ---'
  ! grep -rn --include='*.py' -E 'method=\"(POST|PUT|PATCH|DELETE)\"' apps/agent-service/skybridge_ai/evidence.py apps/agent-service/skybridge_ai/server.py apps/agent-service/skybridge_ai/review.py &&
  echo '--- no cloud SDKs, no terraform, no cloud credentials ---'
  ! grep -rniE 'boto3|aws-sdk|azure-sdk|@aws-sdk|@azure|terraform (apply|destroy)|AWS_SECRET|AZURE_CLIENT_SECRET|management.azure.com|sts.amazonaws.com' apps/agent-service/skybridge_ai/ apps/agent-service/tests/ &&
  echo '--- real-cloud gate still blocked ---'
  grep -q 'ApplyEnabled = false' apps/control-plane/tfvars.go &&
  echo PASS no-mutation-path"

# ---- 4. console integration (read-only AI panel) ----
magnet AI_CONSOLE_INTEGRATION bash -c "
  set -o pipefail
  grep -q 'apiAiReview' '$CONSOLE/src/lib/api.ts' &&
  grep -q 'ADVISORY — DOES NOT AUTHORIZE MIGRATION' '$CONSOLE/src/components/AiReview.tsx' &&
  grep -q 'AiReviewPanel' '$CONSOLE/src/app/migrations/[id]/page.tsx' &&
  grep -q 'AiReviewPanel' '$CONSOLE/src/app/demo/page.tsx' &&
  grep -q 'NEXT_PUBLIC_AI_SERVICE_URL' '$CONSOLE/src/lib/config.ts' &&
  NEXT_PUBLIC_CONTROL_PLANE_URL='$CONTROL_PLANE_URL' NEXT_PUBLIC_AI_SERVICE_URL='$AI_URL' npm --prefix '$CONSOLE' run build 2>&1 | tail -n 3 &&
  (cd '$CONSOLE' && npm test 2>&1 | tail -n 4) &&
  echo PASS ai-console-integration"

if [ "$OVERALL" -eq 0 ]; then
  echo "SKYBRIDGE_AI_ADVISORY = PASS"
else
  echo "SKYBRIDGE_AI_ADVISORY = FAIL (logs in $LOGDIR)"
fi
exit "$OVERALL"
