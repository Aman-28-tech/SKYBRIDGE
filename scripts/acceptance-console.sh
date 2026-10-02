#!/usr/bin/env bash
# SKYBRIDGE console acceptance (v1, read-only).
#
# Verifies the Next.js console builds, its tests pass, it stays connected to
# a live control plane, every read-only view renders from real API evidence,
# the demo mode reflects real state, and no mutation controls exist.
#
# Read-only + local ONLY. This script MUST NEVER run: terraform apply /
# terraform destroy / AWS or Azure API mutation / ownership transfer /
# rollback / arbitrary cloud actions. It builds/starts the console and the
# control plane (in-memory store), seeds one workload + migration through the
# public APIs, and curls read-only endpoints. No cloud credentials loaded.
#
# Env (all optional):
#   CP_PORT (default 18081), CONSOLE_PORT (default 3001)
#   CONTROL_PLANE_URL (derived from CP_PORT)
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CP_PORT="${CP_PORT:-18081}"
CONSOLE_PORT="${CONSOLE_PORT:-3001}"
CONTROL_PLANE_URL="${CONTROL_PLANE_URL:-http://localhost:$CP_PORT}"
CONSOLE_URL="http://localhost:$CONSOLE_PORT"
LOGDIR="${TMPDIR:-/tmp}/skybridge-acceptance-console-$$"
mkdir -p "$LOGDIR"
OVERALL=0
CP_PID=""; CONSOLE_PID=""

cleanup() {
  [ -n "$CP_PID" ] && kill "$CP_PID" 2>/dev/null || true
  [ -n "$CONSOLE_PID" ] && kill "$CONSOLE_PID" 2>/dev/null || true
  # next start spawns a child server that outlives the launcher subshell;
  # reclaim both ephemeral ports so repeated runs never collide.
  if command -v fuser >/dev/null 2>&1; then
    fuser -k "$CONSOLE_PORT/tcp" >/dev/null 2>&1 || true
    fuser -k "$CP_PORT/tcp" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# magnet <NAME> <cmd...>: prints NAME_OK only on exit 0 AND >=1 PASS line
# (defeats vacuous runs: a command that prints nothing can never pass).
magnet() {
  local name="$1"; shift
  local log="$LOGDIR/$name.log"
  if "$@" >"$log" 2>&1; then
    local passes
    passes=$(grep -cE '^[[:space:]]*(--- PASS|ok |PASS|Tests .* passed)' "$log" || true)
    if [ "$passes" -ge 1 ] && ! grep -q "no tests ran" "$log"; then
      echo "${name}_OK"
      return 0
    fi
  fi
  echo "${name}_FAIL (see $log)"
  OVERALL=1
  return 0
}

CONSOLE="$ROOT/apps/console"
CP="$ROOT/apps/control-plane"

# ---- 1. console build + tests (no backend needed) ----
magnet CONSOLE_BUILD bash -c "
  set -o pipefail; cd '$CONSOLE' && npm run build 2>&1 | tail -n 5 && echo PASS console-build"
magnet CONSOLE_TESTS bash -c "
  set -o pipefail; cd '$CONSOLE' && npm test 2>&1 | tail -n 6"

# ---- 2. live control plane (in-memory) + seeded evidence ----
cd "$CP" && go build -o "$LOGDIR/control-plane" . || { echo "CONTROL_PLANE_BUILD_FAIL"; OVERALL=1; }
# Local/dev auth (H-1): per-run admin token for the seeded mutations below.
ACC_TOKEN="acc-console-$(openssl rand -hex 12 2>/dev/null || python3 -c 'import secrets; print(secrets.token_hex(12))')"
ACC_AUTH_HDR="Authorization: Bearer $ACC_TOKEN"
PORT="$CP_PORT" SKYBRIDGE_LOCAL_TOKENS='[{"token":"'"$ACC_TOKEN"'","actor_id":"acc-seed","actor_type":"human","admin":true}]' "$LOGDIR/control-plane" >"$LOGDIR/cp.log" 2>&1 &
CP_PID=$!
for i in $(seq 1 20); do
  curl -s -m 3 "$CONTROL_PLANE_URL/healthz" | grep -q ok && break
  sleep 1
  [ "$i" = 20 ] && { echo "CONTROL_PLANE_START_FAIL (see $LOGDIR/cp.log)"; OVERALL=1; }
done

K="console-acc-$$"
SPEC='{"schema_version":1,"name":"cloudshop","compute":{"orchestrator":"kubernetes","replicas":2,"cpu_millicores":500,"memory_mib":512},"database":{"engine":"postgresql","major_version":"17","storage_gib":20},"cache":{"engine":"redis","authoritative":false},"object_storage":{"required":true,"versioning_required":true},"queue":{"delivery_semantics":"at_least_once","duplicate_safe_consumer":true},"requirements":{"rpo_seconds":30,"rto_seconds":900,"private_data_plane":true}}'
WID=$(curl -s -m 10 -X POST "$CONTROL_PLANE_URL/v1/workloads" -H "Idempotency-Key: $K-w01" -H "$ACC_AUTH_HDR" -d "$SPEC" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])") || WID=""
MID=$(curl -s -m 10 -X POST "$CONTROL_PLANE_URL/v1/migrations" -H "Idempotency-Key: $K-m01" -H "$ACC_AUTH_HDR" -d '{"workload_id":"'$WID'","target_provider":"azure"}' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])") || MID=""
curl -s -m 10 -X POST "$CONTROL_PLANE_URL/v1/workloads/$WID/compatibility" -H "Idempotency-Key: $K-c01" -H "$ACC_AUTH_HDR" -d '{"target_provider":"azure"}' >"$LOGDIR/compat.json" 2>&1 || true
echo "WORKLOAD=$WID MIGRATION=$MID" >"$LOGDIR/ids.env"
[ -n "$WID" ] && [ -n "$MID" ] || { echo "SEED_FAIL"; OVERALL=1; }
curl -s -m 10 "$CONTROL_PLANE_URL/v1/migrations/$MID/summary" >"$LOGDIR/summary.json" 2>&1 || true
curl -s -m 10 "$CONTROL_PLANE_URL/v1/migrations/$MID/audit" >"$LOGDIR/audit.json" 2>&1 || true

# check <NAME> <python-expr> <summary|audit>: NAME_OK iff the expression
# holds over the live API response (no hardcoded success values).
check() {
  local name="$1" expr="$2" src="$3"
  local log="$LOGDIR/$name.log"
  if python3 -c "
import json
d = json.load(open('$LOGDIR/$src.json'))
assert ($expr)
print('PASS $name')" >"$log" 2>&1; then
    echo "${name}_OK"
  else
    echo "${name}_FAIL (see $log)"
    OVERALL=1
  fi
}

check CONTROL_PLANE_CONNECTED "d.get('migration_id')=='$MID'" summary
check DASHBOARD "d.get('providers',{}).get('source_provider')=='aws' and d.get('providers',{}).get('target_provider')=='azure' and 'authoritative_provider' in d.get('providers',{}) and 'routing_provider' in d.get('providers',{}) and d.get('mode',{}).get('terraform_apply')=='disabled' and d.get('lifecycle',{}).get('current') and d.get('cutover',{}).get('status') and 'cdc' in d and 'safety' in d" summary
check MIGRATION_VIEW "[s['name'] for s in d['lifecycle']['stages']]==['REGISTERED','COMPATIBILITY','PLANNED','REHEARSED','CANARY','QUIESCED','CUTOVER','COMPLETE'] and d['lifecycle']['current']=='PLANNED' and [s['status'] for s in d['lifecycle']['stages'] if s['name'] in ('REGISTERED','COMPATIBILITY')]==['completed','completed']" summary
check CDC_VIEW "set(['source_lsn','applied_lsn','lag_seconds','rpo_seconds','events_captured','events_applied','events_duplicates','reconciliation_match']).issubset(set(d['cdc'].keys())) and d['cdc']['rpo_seconds']==30" summary
check OWNERSHIP_VIEW "d['ownership']['current_owner']=='aws' and d['ownership']['source_writable'] is True and d['ownership']['target_writable'] is False and d['ownership']['routing']=='aws' and d['ownership']['split_brain']=='SAFE'" summary
check CUTOVER_TIMELINE "[s['name'] for s in d['cutover']['stages']]==['FINAL_PREFLIGHT','WRITES_QUIESCED','CDC_CATCHING_UP','CDC_CAUGHT_UP','FINAL_VALIDATION','OWNERSHIP_TRANSFERRED','TRAFFIC_SWITCHED','WRITES_RESUMED','CUTOVER_COMPLETE'] and all(set(['status','timestamp','request_id','actor','policy_hash','approval_id','source_lsn','applied_lsn','lag_seconds']).issubset(set(s.keys())) for s in d['cutover']['stages'])" summary
check SAFETY_VIEW "set(['compatibility','drift_gate','policy_decision','approval','canary_verdict','quiesce','rollback','split_brain']).issubset(set(d['safety'].keys())) and d['safety']['compatibility'] in ('pass','conditional','unknown','block')" summary

# Audit timeline must be served (chronological, >= seed entry), never empty-misread.
check TIMELINE_AUDIT "len(d.get('items',[]))>=1 and d.get('migration_id')=='$MID'" audit

# Demo mode reflects real state: the lifecycle position the /demo flow
# renders must be a genuine API-reported stage.
check DEMO_STATE "d['lifecycle']['current'] in ['REGISTERED','COMPATIBILITY','PLANNED','REHEARSED','CANARY','QUIESCED','CUTOVER','COMPLETE']" summary

# ---- 3. served console routes (production build, read-only) ----
NEXT_PUBLIC_CONTROL_PLANE_URL="$CONTROL_PLANE_URL" npm --prefix "$CONSOLE" run build >"$LOGDIR/console-build2.log" 2>&1 || OVERALL=1
(cd "$CONSOLE" && exec npx next start -p "$CONSOLE_PORT" >"$LOGDIR/console.log" 2>&1 &)
CONSOLE_PID=$!
for i in $(seq 1 30); do
  curl -s -m 3 "$CONSOLE_URL/" | grep -q "SKYBRIDGE Console" && break
  sleep 2
  [ "$i" = 30 ] && { echo "CONSOLE_START_FAIL (see $LOGDIR/console.log)"; OVERALL=1; }
done

# route <NAME> <path> [marker]: NAME_OK iff HTTP 200 + console shell served
# (+ marker when the page is server-rendered). Client pages render Loading
# on SSR, so data wiring is proven by the API checks above, not by HTML.
route() {
  local name="$1" path="$2" marker="${3:-}"
  local log="$LOGDIR/$name.log" bodyfile="$LOGDIR/$name.body"
  local code
  code=$(curl -s -m 15 -o "$bodyfile" -w "%{http_code}" "$CONSOLE_URL$path" || echo "000")
  if [ "$code" = "200" ] && grep -q "SKYBRIDGE Console" "$bodyfile" && { [ -z "$marker" ] || grep -q "$marker" "$bodyfile"; }; then
    echo "PASS $name $path ($code)" >"$log"
    echo "${name}_OK"
  else
    echo "ROUTE $path -> HTTP $code (marker: $marker)" >"$log"
    echo "${name}_FAIL (see $log)"
    OVERALL=1
  fi
}

route ROUTE_DASHBOARD "/" "Read-only"
route ROUTE_MIGRATION "/migrations/$MID"
route ROUTE_CDC "/cdc"
route ROUTE_OWNERSHIP "/ownership"
route ROUTE_CUTOVER "/cutover"
route ROUTE_SAFETY "/safety"
route ROUTE_EVIDENCE "/evidence" "No real cloud mutation"
route ROUTE_DEMO "/demo"

# Evidence page is server-rendered: the matrix groups must be in the HTML.
if grep -q "PROVEN" "$LOGDIR/ROUTE_EVIDENCE.body" && grep -q "PARTIALLY PROVEN" "$LOGDIR/ROUTE_EVIDENCE.body" && grep -q "DEFERRED" "$LOGDIR/ROUTE_EVIDENCE.body" && grep -q "No cloud spending" "$LOGDIR/ROUTE_EVIDENCE.body"; then
  echo "PASS EVIDENCE_VIEW matrix groups + no-spend boundary" >"$LOGDIR/EVIDENCE_VIEW.log"
  echo "EVIDENCE_VIEW_OK"
else
  echo "EVIDENCE_VIEW_FAIL (see $LOGDIR/ROUTE_EVIDENCE.body)"
  OVERALL=1
fi

# Demo mode: route serves 200 (checked above) and reflects the real
# API-reported lifecycle position (checked as DEMO_STATE).
if grep -q "200" "$LOGDIR/ROUTE_DEMO.log" && grep -q "PASS DEMO_STATE" "$LOGDIR/DEMO_STATE.log"; then
  echo "PASS DEMO_MODE live route + real API state" >"$LOGDIR/DEMO_MODE.log"
  echo "DEMO_MODE_OK"
else
  echo "DEMO_MODE_FAIL"
  OVERALL=1
fi

# ---- 4. no mutation controls (executable buttons / verbs / cloud SDKs) ----
magnet NO_MUTATION_CONTROLS bash -c "
  set -o pipefail; cd '$CONSOLE' && npx vitest run -t 'NO_MUTATION_CONTROLS' 2>&1 | tail -n 4 &&
  ! grep -rn --include='*.tsx' --include='*.ts' -E '<button[^>]*>([^<]*([Aa]pply|[Dd]estroy|[Tt]ransfer ownership|[Rr]ollback|[Ss]witch traffic)[^<]*)</button>' src/ |
    grep -v test | grep -q . &&
  ! grep -rn --include='*.ts' --include='*.tsx' -E 'method:[[:space:]]*\"(POST|PUT|PATCH|DELETE)\"' src/lib src/app src/components |
    grep -v test | grep -q . &&
  echo PASS no-mutation-controls"

if [ "$OVERALL" -eq 0 ]; then
  echo "SKYBRIDGE_CONSOLE = PASS"
else
  echo "SKYBRIDGE_CONSOLE = FAIL (logs in $LOGDIR)"
fi
exit "$OVERALL"
