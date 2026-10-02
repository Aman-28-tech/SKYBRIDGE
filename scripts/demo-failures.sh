#!/usr/bin/env bash
# SKYBRIDGE local failure matrix: deterministic fault injection proving the
# existing safety behavior. Live scenarios run against the lab; scenarios
# that cannot be forced live without timing hacks run their exact unit
# proofs instead (never faked metrics). Every scenario prints
# SCENARIO / INJECTED_FAILURE / EXPECTED_BEHAVIOR / ACTUAL_BEHAVIOR /
# RESULT=PASS|FAIL. Stateful scenarios use fresh chains; the lab is left
# AWS-authoritative with services running.
#
# Env: CONTROL_PLANE_URL/SHOP_SRC_URL/SHOP_TGT_URL/COMPOSE/RUN_DIR as in
# demo-migration.sh; DEMO_RUN_ID for key determinism.
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUN_ID="${DEMO_RUN_ID:-$(date +%s)}"
CONTROL_PLANE_URL="${CONTROL_PLANE_URL:-http://localhost:18080}"
COMPOSE="${COMPOSE:-docker compose}"
# Per-invocation nonce baked into K: the control-plane is NOT restarted by
# this script (it reuses the running instance), so reusing operation keys
# across invocations would replay previous runs' stored responses (same key
# + changed body = 409 conflict; same key + same body = stale entity reuse).
# Rehearsal keys additionally carry an explicit -$INV suffix (stale-capture
# hazard documented in demo-migration.sh: probe IDs and the Redpanda
# consumer group derive from the rehearsal key, and groups persist on the
# broker). Scenario J's proof is unaffected: its two concurrent cutovers
# still share one identical key within the run.
INV="$(date +%s)-$$"
echo "INVOCATION=$INV"
# Local/dev auth (H-1): reuse this lab's per-run tokens minted by
# demo-migration.sh ($ROOT/.demo-run/.auth-tokens). Mutations present
# `Authorization: Bearer`; X-Actor-Id headers are no longer trusted.
if [ -f "$ROOT/.demo-run/.auth-tokens" ]; then
  # shellcheck disable=SC1091
  . "$ROOT/.demo-run/.auth-tokens"
fi
[ -n "${CP_REQUESTER_TOKEN:-}" ] && [ -n "${CP_APPROVER_TOKEN:-}" ] && [ -n "${SHOP_ADMIN_TOKEN:-}" ] || {
  echo "AUTH_TOKENS_MISSING: run scripts/demo-migration.sh first (it mints per-run local credentials)"
  exit 1
}
CP_AUTH_HDR="Authorization: Bearer $CP_REQUESTER_TOKEN"
SHOP_AUTH_HDR="Authorization: Bearer $SHOP_ADMIN_TOKEN"
K="demofail-${RUN_ID}-${INV}"
N=0
# Result table: scenario() queues one row per scenario; result() stamps the
# shared actual/result onto all queued rows (H+I and K+L share one unit
# proof, so one result() call closes two rows). A trap prints the compact
# table on every exit path — failures are never hidden, exit codes unchanged.
mkdir -p "$ROOT/.demo-run" 2>/dev/null || true
TABLE_FILE="$ROOT/.demo-run/failures-table.txt"
PENDING_FILE="$ROOT/.demo-run/failures-pending.txt"
: >"$TABLE_FILE" 2>/dev/null || true
: >"$PENDING_FILE" 2>/dev/null || true
scenario() { N=$((N+1)); echo "SCENARIO $1"; echo "INJECTED_FAILURE: $2"; echo "EXPECTED_BEHAVIOR: $3"; echo "$1|$2|$3" >>"$PENDING_FILE"; }
result() {
  echo "ACTUAL_BEHAVIOR: $1"; echo "RESULT=$2"
  while IFS= read -r line || [ -n "$line" ]; do
    [ -n "$line" ] && echo "$line|$1|$2" >>"$TABLE_FILE"
  done <"$PENDING_FILE"
  : >"$PENDING_FILE"
  [ "$2" = "PASS" ] || exit 1
}
print_table() {
  [ -s "$TABLE_FILE" ] || return 0
  echo ""
  echo "SCENARIO   FAILURE                        EXPECTED                       RESULT"
  awk -F'|' '{printf "%-10s %-30.30s %-30.30s %s\n", $1, $2, $3, $5}' "$TABLE_FILE"
}
finish() { code=$?; print_table; exit "$code"; }
trap finish EXIT
api() {
  local method="$1" path="$2" key="$3" body="${4:-}"
  if [ -n "$body" ]; then
    curl -s -m 60 -X "$method" "$CONTROL_PLANE_URL$path" -H "Idempotency-Key: $key" -H "$CP_AUTH_HDR" -d "$body"
  else
    curl -s -m 15 -X "$method" "$CONTROL_PLANE_URL$path" -H "Idempotency-Key: $key" -H "$CP_AUTH_HDR"
  fi
}
SPEC='{"schema_version":1,"name":"cloudshop","compute":{"orchestrator":"kubernetes","replicas":2,"cpu_millicores":500,"memory_mib":512},"database":{"engine":"postgresql","major_version":"17","storage_gib":20},"cache":{"engine":"redis","authoritative":false},"object_storage":{"required":true,"versioning_required":true},"queue":{"delivery_semantics":"at_least_once","duplicate_safe_consumer":true},"requirements":{"rpo_seconds":30,"rto_seconds":900,"private_data_plane":true}}'
mkchain() { # prefix -> "wid mid" (register/compat/plan/clean-drift/migration)
  local p="$1" W M
  W=$(api POST /v1/workloads "$K-$p-w" "$SPEC" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
  M=$(api POST /v1/migrations "$K-$p-m" '{"workload_id":"'$W'","target_provider":"azure"}' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
  api POST /v1/workloads/$W/compatibility "$K-$p-c" '{"target_provider":"azure"}' >/dev/null
  api POST /v1/workloads/$W/plan "$K-$p-p" '{"target_provider":"azure"}' >/dev/null
  curl -s -m 15 -X POST "$CONTROL_PLANE_URL/v1/workloads/$W/drift" -H "Idempotency-Key: $K-$p-d" -H "$CP_AUTH_HDR" \
    -H 'Content-Type: application/json' -d @"$ROOT/tests/fixtures/drift/clean.json" >/dev/null
  echo "$W $M"
}
canary_walk() { # wid mid prefix
  # Per-stage observed metrics with the stage-gated minimum volume/windows
  # (stage 50 requires window_secs >= 3600; a single BASE payload for every
  # stage leaves stage 50 without a PASS record and preflight BLOCKED).
  # Values mirror the proven demo-migration.sh walk.
  local W="$1" M="$2" p="$3"
  local BASE='{"error_rate_5xx":0.002,"p95_ms":200,"p99_ms":400,"requests":500,"window_secs":900,"healthy":true}'
  local i=0 s e OBS
  for s in 0 1 5 25 50; do
    case $s in 0) e=0;; 1) e=1;; 5) e=5;; 25) e=25;; 50) e=50;; esac
    case $s in
      0)  OBS='{"error_rate_5xx":0.002,"p95_ms":200,"p99_ms":400,"requests":500,"window_secs":900,"healthy":true}';;
      1)  OBS='{"error_rate_5xx":0.003,"p95_ms":210,"p99_ms":420,"requests":150,"window_secs":300,"healthy":true}';;
      5)  OBS='{"error_rate_5xx":0.004,"p95_ms":220,"p99_ms":430,"requests":400,"window_secs":900,"healthy":true}';;
      25) OBS='{"error_rate_5xx":0.004,"p95_ms":230,"p99_ms":440,"requests":400,"window_secs":1800,"healthy":true}';;
      50) OBS='{"error_rate_5xx":0.005,"p95_ms":240,"p99_ms":450,"requests":400,"window_secs":3600,"healthy":true}';;
    esac
    api POST /v1/migrations/$M/canary "$K-$p-can$i" \
      '{"stage":'$s',"expected_stage":'$e',"baseline":'$BASE',"observed":'$OBS'}' >/dev/null
    i=$((i+1))
  done
}
approve_at_lag() { # wid lag prefix -> aid
  local W="$1" lag="$2" p="$3"
  local RT='{"target_weight":0,"environment":"dev","validation_status":"passed","cdc_lag_seconds":'$lag',"target_healthy":true,"read_only_canary":true,"write_ownership":"aws"}'
  local A
  A=$(curl -s -m 15 -X POST "$CONTROL_PLANE_URL/v1/workloads/$W/approvals" -H "Idempotency-Key: $K-$p-a1" -H "Authorization: Bearer $CP_REQUESTER_TOKEN" -d "$RT" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
  curl -s -m 15 -X POST "$CONTROL_PLANE_URL/v1/workloads/$W/approvals/$A/decision" -H "Idempotency-Key: $K-$p-a2" -H "Authorization: Bearer $CP_APPROVER_TOKEN" -d '{"decision":"approved"}' >/dev/null
  echo "$A"
}

curl -s -m 8 "$CONTROL_PLANE_URL/healthz" | grep -q ok || { echo "control-plane unreachable; start the demo lab first"; exit 1; }

# Baseline: prior runs may leave shops azure-named/quiesced; restore the
# AWS-authoritative starting state (best effort, verified per scenario).
for base in "${SHOP_SRC_URL:-http://localhost:8085}" "${SHOP_TGT_URL:-http://localhost:8086}"; do
  curl -s -m 8 -H "$SHOP_AUTH_HDR" -X POST "$base/v1/admin/ownership" -d '{"write_ownership":"aws"}' >/dev/null 2>&1 || true
  curl -s -m 8 -H "$SHOP_AUTH_HDR" -X POST "$base/v1/admin/quiesce" -d '{"quiesced":false}' >/dev/null 2>&1 || true
done

scenario "A" "target database stopped" "readiness NOT_READY, no transfer, service recovers"
read W M <<<"$(mkchain fa)"
$COMPOSE -f "$ROOT/docker-compose.yml" stop cloudshop-target-db >/dev/null 2>&1
R=$(api POST /v1/migrations/$M/readiness "$K-fa-r" '{"environment":"dev"}')
$COMPOSE -f "$ROOT/docker-compose.yml" start cloudshop-target-db >/dev/null 2>&1
sleep 6
echo "$R" | python3 -c "import sys,json; d=json.load(sys.stdin); assert d['status']=='NOT_READY', d['status']" \
  && result "NOT_READY during outage; target restarted healthy" PASS || result "unexpected" FAIL

scenario "B" "CDC consumer (connect) stopped" "rehearsal BLOCKED CDC_UNAVAILABLE, resumes after restart"
read W M <<<"$(mkchain fb)"
$COMPOSE -f "$ROOT/docker-compose.yml" stop connect >/dev/null 2>&1
R=$(api POST /v1/migrations/$M/rehearse "$K-fb-r-$INV" '{"environment":"dev","target_weight":0,"timeout_secs":15}')
$COMPOSE -f "$ROOT/docker-compose.yml" start connect >/dev/null 2>&1
# Wait for the connector tasks to recover: rehearsing while tasks restart
# would measure a genuine (but incidental) lag breach instead of the
# scenario under test, and later CDC scenarios assume a healthy pipeline.
recovered=""
for i in $(seq 1 60); do
  if curl -s -m 5 http://localhost:8083/connectors/cloudshop-pg-connector/status 2>/dev/null | python3 -c "
import sys, json
d = json.load(sys.stdin)
assert d['connector']['state'] == 'RUNNING', d['connector']
assert d.get('tasks') and all(t['state'] == 'RUNNING' for t in d['tasks']), d.get('tasks')
" 2>/dev/null; then recovered="yes"; break
  fi
  sleep 2
done
[ "$recovered" = "yes" ] || { echo "connect did not recover"; exit 1; }
echo "$R" | python3 -c "import sys,json; d=json.load(sys.stdin); assert d['status']=='REHEARSAL_BLOCKED', d['status']" \
  && result "BLOCKED without capture; connect restarted" PASS || result "unexpected" FAIL

scenario "C" "CDC lag above 30s RPO" "deny with rpo_breach (unit proof: real 30s stall impractical)"
if (cd "$ROOT/apps/control-plane" && go test -count=1 -run 'TestReadinessLagBreach|TestCutoverLagBreach' ./... >/dev/null 2>&1); then
  result "lag 45s denied, RPO decision rpo_breach" PASS
else result "unit proof failed" FAIL; fi

scenario "D" "reconciliation mismatch" "validation_failed, no transfer (unit proof)"
if (cd "$ROOT/apps/control-plane" && go test -count=1 -run 'TestReadinessReconMismatch|TestCutoverReconMismatch|TestRehearsalReconMismatch' ./... >/dev/null 2>&1); then
  result "mismatch blocks readiness/rehearsal/cutover" PASS
else result "unit proof failed" FAIL; fi

scenario "E" "canary 5xx breach" "stage FAIL, advancement refused with STAGE_FAILED"
read W M <<<"$(mkchain fe)"
BASE='{"error_rate_5xx":0.002,"p95_ms":200,"p99_ms":400,"requests":500,"window_secs":900,"healthy":true}'
api POST /v1/migrations/$M/canary "$K-fe-0" '{"stage":0,"expected_stage":0,"baseline":'$BASE',"observed":'$BASE'}' >/dev/null
BAD='{"error_rate_5xx":0.09,"p95_ms":210,"p99_ms":420,"requests":150,"window_secs":300,"healthy":true}'
R1=$(api POST /v1/migrations/$M/canary "$K-fe-1" '{"stage":1,"expected_stage":1,"baseline":'$BASE',"observed":'$BAD'}')
R2=$(api POST /v1/migrations/$M/canary "$K-fe-2" '{"stage":5,"expected_stage":5,"baseline":'$BASE',"observed":'$BASE'}')
echo "$R1" | grep -q '"verdict":"FAIL"' && echo "$R2" | grep -q 'STAGE_FAILED' \
  && result "FAIL recorded; advance refused" PASS || result "unexpected: $R1 $R2" FAIL

scenario "F" "approval missing on conditional path" "APPROVAL_REQUIRED, no transfer"
read W M <<<"$(mkchain ff)"
R=$(api POST /v1/migrations/$M/rehearse "$K-ff-r-$INV" '{"environment":"dev","target_weight":0}')
echo "$R" | grep -q 'APPROVAL_REQUIRED' \
  && result "blocked pending explicit approval" PASS || result "unexpected: $R" FAIL

scenario "G" "blocking drift" "policy deny before any data-plane work"
read W M <<<"$(mkchain fg)"
curl -s -m 15 -X POST "$CONTROL_PLANE_URL/v1/workloads/$W/drift" -H "Idempotency-Key: $K-fg-d2" -H "$CP_AUTH_HDR" \
  -H 'Content-Type: application/json' -d @"$ROOT/tests/fixtures/drift/versioning-off.json" >/dev/null
R=$(api POST /v1/migrations/$M/rehearse "$K-fg-r-$INV" '{"environment":"dev","target_weight":0}')
echo "$R" | grep -q 'blocking_drift' \
  && result "blocked with blocking_drift" PASS || result "unexpected" FAIL

scenario "H" "stale compatibility evidence" "COMPATIBILITY_STALE (unit proof)"
scenario "I" "stale plan evidence" "PLAN_STALE (unit proof)"
if (cd "$ROOT/apps/control-plane" && go test -count=1 -run 'TestRehearsalStaleCompat|TestRehearsalStalePlanner|TestCutoverStaleEvidence' ./... >/dev/null 2>&1); then
  result "staleness invalidates transitions" PASS
else result "unit proof failed" FAIL; fi

scenario "J" "concurrent cutover requests (same key)" "single transfer, identical bytes"
read W M <<<"$(mkchain fj)"
canary_walk "$W" "$M" "fj"
curl -s -m 20 -X POST "$CONTROL_PLANE_URL/v1/migrations/$M/quiesce" -H "Idempotency-Key: $K-fj-q" -H "$CP_AUTH_HDR" -d '{"quiesced":true}' >/dev/null
LAG=$(api POST /v1/migrations/$M/rehearse "$K-fj-h-$INV" '{"environment":"dev","target_weight":0}' | python3 -c "import sys,json; print((json.load(sys.stdin).get('cdc') or {}).get('cdc_lag_seconds') or 8)")
AID=$(approve_at_lag "$W" "$LAG" "fj")
# Approval binds the measured evidence; lag quantization jitter yields a
# correct STALE, so converge with the demo's nested loop (inner: same
# approval absorbs oscillation with fresh verify keys; outer: re-approval
# adapts to drift; exhaustion fails loudly at the cutovers below).
VATTEMPT=0; VSEQ=0
while [ "$VATTEMPT" -lt 2 ]; do
  VATTEMPT=$((VATTEMPT+1))
  for VINNER in 1 2 3; do
    VSEQ=$((VSEQ+1))
    VERIFY=$(api POST /v1/migrations/$M/rehearse "$K-fj-v$VSEQ-$INV" '{"environment":"dev","target_weight":0,"approval_id":"'$AID'"}')
    if [ "$(echo "$VERIFY" | python3 -c "import sys,json; print(json.load(sys.stdin)['status'])")" = "REHEARSAL_READY" ]; then break 2; fi
    LAG2=$(echo "$VERIFY" | python3 -c "import sys,json; print((json.load(sys.stdin).get('cdc') or {}).get('cdc_lag_seconds') or 8)")
  done
  AID=$(approve_at_lag "$W" "$LAG2" "fj$VATTEMPT")
done
BODY='{"environment":"dev","approval_id":"'$AID'"}'
# Lag jitter can stale the approval between approval and cutover: both
# requests then block identically at preflight (fail-closed, no mutation).
# Converge like demo-migration step 17 — retry the concurrent same-key pair
# with a fresh lag probe, fresh approval, and fresh keys per attempt (same
# key + new approval would 409). The property under test (same key ->
# identical bytes, exactly one transfer) holds on every attempt; exhaustion
# fails loudly.
JATTEMPT=0
while [ "$JATTEMPT" -lt 3 ]; do
  JATTEMPT=$((JATTEMPT+1))
  JKEY="$K-fj-f$JATTEMPT"
  RUN_DIR_F_j1="$ROOT/.demo-run/fj1.json"
  api POST /v1/migrations/$M/cutover/final "$JKEY" "$BODY" >"$RUN_DIR_F_j1" &
  P1=$!
  api POST /v1/migrations/$M/cutover/final "$JKEY" "$BODY" >"$ROOT/.demo-run/fj2.json" &
  P2=$!
  wait $P1; wait $P2
  if [ "$(python3 -c "import json; print(json.load(open('$ROOT/.demo-run/fj1.json'))['status'])")" = "CUTOVER_COMPLETE" ]; then break; fi
  echo "scenario J attempt $JATTEMPT blocked identically; re-probing lag and re-approving"
  LAG=$(api POST /v1/migrations/$M/rehearse "$K-fj-h$JATTEMPT-$INV" '{"environment":"dev","target_weight":0}' | python3 -c "import sys,json; print((json.load(sys.stdin).get('cdc') or {}).get('cdc_lag_seconds') or 8)")
  AID=$(approve_at_lag "$W" "$LAG" "fjj$JATTEMPT")
  BODY='{"environment":"dev","approval_id":"'$AID'"}'
done
if python3 -c "import json; a=json.load(open('$ROOT/.demo-run/fj1.json')); b=json.load(open('$ROOT/.demo-run/fj2.json')); assert a==b and a['status']=='CUTOVER_COMPLETE', (a.get('status'), b.get('status'))"; then
  result "one transfer, byte-identical responses" PASS
else result "diverged or incomplete" FAIL; fi
# restore lab ownership for later scenarios (fresh control-plane state comes
# from the demo reset; CloudShop flags follow)
curl -s -m 8 -H "$SHOP_AUTH_HDR" -X POST "${SHOP_SRC_URL:-http://localhost:8085}/v1/admin/ownership" -d '{"write_ownership":"aws"}' >/dev/null 2>&1 || true
curl -s -m 8 -H "$SHOP_AUTH_HDR" -X POST "${SHOP_SRC_URL:-http://localhost:8085}/v1/admin/quiesce" -d '{"quiesced":false}' >/dev/null 2>&1 || true
curl -s -m 8 -H "$SHOP_AUTH_HDR" -X POST "${SHOP_TGT_URL:-http://localhost:8086}/v1/admin/ownership" -d '{"write_ownership":"aws"}' >/dev/null 2>&1 || true
curl -s -m 8 -H "$SHOP_AUTH_HDR" -X POST "${SHOP_TGT_URL:-http://localhost:8086}/v1/admin/quiesce" -d '{"quiesced":false}' >/dev/null 2>&1 || true

scenario "K" "worker restart during cutover" "resume to exactly one transfer (unit proof)"
scenario "L" "partial ownership-transfer failure" "paused-safe, resume completes (unit proof)"
if (cd "$ROOT/apps/control-plane" && go test -count=1 -run 'TestCutoverWorkerRestart|TestCutoverTargetFlipFailure|TestCutoverVerifyFailure|TestCutoverResumeFailure|TestCutoverQuiesceTransitionFailure|TestCutoverCDCFailure' ./... >/dev/null 2>&1); then
  result "restart/resume/partial paths proven" PASS
else result "unit proof failed" FAIL; fi

echo "FAILURE_MATRIX = COMPLETE"
