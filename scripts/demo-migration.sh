#!/usr/bin/env bash
# SKYBRIDGE free local multi-cloud migration demo: cloudshop-local-aws-to-azure.
#
# Exercises the REAL control-plane APIs (register, compatibility, plan,
# drift, readiness, approvals, rehearse, canary, quiesce, cutover/final)
# against local CloudShop source/target plus real Debezium/Redpanda CDC.
# No cloud credentials, no spend, no Terraform apply.
#
# Env (all optional):
#   DEMO_RUN_ID      deterministic run tag (default: timestamp)
#   CP_PORT/SRC_PORT/TGT_PORT (defaults 18080/8085/8086)
#   CONTROL_PLANE_URL/SHOP_SRC_URL/SHOP_TGT_URL (derived from ports)
#   COMPOSE          compose command (default "docker compose")
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUN_ID="${DEMO_RUN_ID:-$(date +%s)}"
CP_PORT="${CP_PORT:-18080}"
SRC_PORT="${SRC_PORT:-8085}"
TGT_PORT="${TGT_PORT:-8086}"
CONTROL_PLANE_URL="${CONTROL_PLANE_URL:-http://localhost:$CP_PORT}"
SHOP_SRC_URL="${SHOP_SRC_URL:-http://localhost:$SRC_PORT}"
SHOP_TGT_URL="${SHOP_TGT_URL:-http://localhost:$TGT_PORT}"
COMPOSE="${COMPOSE:-docker compose}"
RUN_DIR="$ROOT/.demo-run"
mkdir -p "$RUN_DIR"
K="demorun-${RUN_ID}"
# Per-invocation nonce for CDC-touching rehearsal keys. Rehearsal probe IDs
# and the Redpanda consumer group both derive from the HTTP idempotency key,
# and consumer groups persist on the broker across demo invocations: reusing
# a rehearsal key in a later invocation (same RUN_ID) would make awaitProbes
# match the previous invocation's probe records (same IDs) and report
# genuinely stale lag, which the RPO gate then correctly rejects.
# Data IDs (DEMO_USER etc.) stay deterministic per RUN_ID; only these
# operation keys carry the nonce. Printed for traceability.
INV="$(date +%s)-$$"
echo "INVOCATION=$INV"

# Local/dev authentication (H-1 remediation): mint per-run random
# credentials. No secret is committed anywhere: tokens live only in this
# process tree and the servers it starts, and every mutation presents them
# as `Authorization: Bearer`. The control plane verifies tokens server-side
# (X-Actor-Id headers are no longer trusted); it forwards the shop token to
# CloudShop admin calls. Sibling scripts read $RUN_DIR/.auth-tokens.
randhex() { openssl rand -hex 12 2>/dev/null || python3 -c "import secrets; print(secrets.token_hex(12))"; }
CP_REQUESTER_TOKEN="demo-req-$(randhex)"
CP_APPROVER_TOKEN="demo-app-$(randhex)"
SHOP_ADMIN_TOKEN="demo-shop-$(randhex)"
export SKYBRIDGE_LOCAL_TOKENS='[{"token":"'"$CP_REQUESTER_TOKEN"'","actor_id":"requester-1","actor_type":"human","admin":true},{"token":"'"$CP_APPROVER_TOKEN"'","actor_id":"approver-1","actor_type":"human","admin":true}]'
export CLOUDSHOP_ADMIN_TOKENS='[{"token":"'"$SHOP_ADMIN_TOKEN"'","actor_id":"skybridge-control-plane","admin":true}]'
export CLOUDSHOP_ADMIN_TOKEN="$SHOP_ADMIN_TOKEN"
cat >"$RUN_DIR/.auth-tokens" <<EOF
CP_REQUESTER_TOKEN="$CP_REQUESTER_TOKEN"
CP_APPROVER_TOKEN="$CP_APPROVER_TOKEN"
SHOP_ADMIN_TOKEN="$SHOP_ADMIN_TOKEN"
CLOUDSHOP_ADMIN_TOKEN="$SHOP_ADMIN_TOKEN"
EOF
chmod 600 "$RUN_DIR/.auth-tokens"
echo "AUTH_TOKENS_MINTED (per-run, local-only)"

fail() { echo "DEMO_FAIL: $1"; stop_spawned; exit 1; }
stop_spawned() { # best-effort: stop demo processes we started (failure path only)
  for pidfile in "$RUN_DIR/cloudshop-src.pid" "$RUN_DIR/cloudshop-tgt.pid" "$RUN_DIR/control-plane.pid"; do
    if [ -f "$pidfile" ]; then
      pid="$(cat "$pidfile" 2>/dev/null || true)"
      if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then kill "$pid" 2>/dev/null || true; fi
    fi
  done
}
step() { echo "== $1 =="; }
phase() { echo ""; echo "[$1]"; }
need() { # python-expr jsonfile-or-- message
  local expr="$1" src="$2" msg="$3" val
  if [ "$src" = "--" ]; then val="$(cat)"; else val="$(cat "$src")"; fi
  if ! echo "$val" | python3 -c "import sys,json; d=json.load(sys.stdin); assert ($expr), '''$msg'''" 2>/dev/null; then
    echo "ASSERT_FAIL: $msg"; echo "$val" | head -c 800; echo; exit 1
  fi
}
api() { # method path key body(enemy)
  local method="$1" path="$2" key="$3" body="${4:-}"
  if [ -n "$body" ]; then
    curl -s -m 30 -X "$method" "$CONTROL_PLANE_URL$path" -H "Idempotency-Key: $key" -H "Authorization: Bearer $CP_REQUESTER_TOKEN" -d "$body"
  else
    curl -s -m 15 -X "$method" "$CONTROL_PLANE_URL$path" -H "Idempotency-Key: $key" -H "Authorization: Bearer $CP_REQUESTER_TOKEN"
  fi
}
psql_src() { $COMPOSE -f "$ROOT/docker-compose.yml" exec -T cloudshop-db psql -U cloudshop -d cloudshop -t -c "$1"; }
psql_tgt() { $COMPOSE -f "$ROOT/docker-compose.yml" exec -T cloudshop-target-db psql -U cloudshop -d cloudshop -t -c "$1"; }

phase "SETUP"
step "1. prerequisites"
command -v go >/dev/null || fail "go missing"
command -v python3 >/dev/null || fail "python3 missing"
command -v curl >/dev/null || fail "curl missing"
$COMPOSE -f "$ROOT/docker-compose.yml" ps cloudshop-db >/dev/null 2>&1 || fail "compose lab unreachable"
for svc in cloudshop-db cloudshop-target-db redpanda; do
  $COMPOSE -f "$ROOT/docker-compose.yml" ps "$svc" 2>/dev/null | grep -q "Up\|running" || fail "$svc not running"
done
curl -s -m 8 http://localhost:8083/connectors >/dev/null 2>&1 || fail "debezium connect unreachable"
echo "PREREQUISITES_OK"

step "2. reset local demo state"
"$ROOT/scripts/reset-demo.sh" >/dev/null

step "3. start/check local services"
cd "$ROOT/apps/control-plane" && go build -o "$RUN_DIR/control-plane" . || fail "control-plane build"
cd "$ROOT/workloads/cloudshop/api" && go build -o "$RUN_DIR/cloudshop" . || fail "cloudshop build"
cd "$ROOT/apps/cdc-applier" && go build -o "$RUN_DIR/cdc-catchup" ./cmd/cdc-catchup || fail "cdc-catchup build"
cd "$ROOT"
DATABASE_URL="postgres://cloudshop:cloudshop@localhost:5433/cloudshop?sslmode=disable" PORT="$SRC_PORT" DEPLOYMENT=aws WRITE_OWNERSHIP=aws nohup "$RUN_DIR/cloudshop" >"$RUN_DIR/shop-src.log" 2>&1 &
echo $! >"$RUN_DIR/cloudshop-src.pid"
DATABASE_URL="postgres://cloudshop:cloudshop@localhost:5434/cloudshop?sslmode=disable" PORT="$TGT_PORT" DEPLOYMENT=azure WRITE_OWNERSHIP=aws nohup "$RUN_DIR/cloudshop" >"$RUN_DIR/shop-tgt.log" 2>&1 &
echo $! >"$RUN_DIR/cloudshop-tgt.pid"
REDPANDA_PROXY_URL=http://localhost:8082 CLOUDSHOP_BASE_URL="$SHOP_SRC_URL" CLOUDSHOP_TARGET_BASE_URL="$SHOP_TGT_URL" PORT="$CP_PORT" nohup "$RUN_DIR/control-plane" >"$RUN_DIR/cp.log" 2>&1 &
echo $! >"$RUN_DIR/control-plane.pid"
for i in $(seq 1 20); do
  curl -s -m 3 "$CONTROL_PLANE_URL/healthz" | grep -q ok && break
  sleep 2
  [ "$i" = 20 ] && fail "control-plane did not start"
done
curl -s -m 8 "$SHOP_SRC_URL/readyz" | grep -q '"ready":true' || fail "source shop not ready"
curl -s -m 8 "$SHOP_TGT_URL/readyz" | grep -q '"ready":true' || fail "target shop not ready"
echo "SERVICES_OK"

phase "WORKLOAD"
step "4. register workload (cloudshop-local-aws-to-azure)"
SPEC='{"schema_version":1,"name":"cloudshop","compute":{"orchestrator":"kubernetes","replicas":2,"cpu_millicores":500,"memory_mib":512},"database":{"engine":"postgresql","major_version":"17","storage_gib":20},"cache":{"engine":"redis","authoritative":false},"object_storage":{"required":true,"versioning_required":true},"queue":{"delivery_semantics":"at_least_once","duplicate_safe_consumer":true},"requirements":{"rpo_seconds":30,"rto_seconds":900,"private_data_plane":true}}'
WID=$(api POST /v1/workloads "$K-w01" "$SPEC" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
MID=$(api POST /v1/migrations "$K-m01" '{"workload_id":"'$WID'","target_provider":"azure"}' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
[ -n "$WID" ] && [ -n "$MID" ] || fail "register"
echo "WORKLOAD=$WID MIGRATION=$MID"

phase "CDC"
step "5. generate source traffic (deterministic users/orders)"
# Deterministic RFC-4122 UUID: sha256 hex digest of RUN_ID is hex-only
# ([0-9a-f]), so the suffix can never inject non-hex chars like "mo01".
# Same RUN_ID -> same UUID across resets (reproducible); distinct RUN_IDs
# -> distinct UUIDs (no collision on users_email_key).
HEX_SUFFIX=$(python3 -c "import hashlib,sys; print(hashlib.sha256(sys.argv[1].encode()).hexdigest()[:4])" "$RUN_ID")
DEMO_USER="aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaa${HEX_SUFFIX}"
psql_src "INSERT INTO users (id, email) VALUES ('$DEMO_USER', 'demo-$RUN_ID@example.invalid') ON CONFLICT (id) DO NOTHING;" >/dev/null || fail "seed user"
# Retry once-prone startup transients: keys+bodies are deterministic and the
# server enforces idempotency, so a retry can never create a duplicate order.
for i in 1 2 3; do
  ok=""
  LAST=""
  for attempt in 1 2 3; do
    LAST=$(curl -s -m 10 -X POST "$SHOP_SRC_URL/v1/orders" -H "Idempotency-Key: $K-o$i" \
      -d '{"user_id":"'$DEMO_USER'","total_cents":'$((100 * i))'}')
    # Semantic JSON check (not grep): idempotency replays are served from
    # Postgres jsonb, which formats as '"status": "pending"' (with space),
    # while fresh writes are Go-compact '"status":"pending"'. Both are the
    # same pending order.
    if echo "$LAST" | python3 -c "import sys,json; assert json.load(sys.stdin)['status']=='pending'" 2>/dev/null; then
      ok="yes"; break
    fi
    sleep 2
  done
  if [ "$ok" != "yes" ]; then echo "LAST_RESPONSE_ORDER_$i: $LAST"; fail "order $i"; fi
done
echo "TRAFFIC_OK user=$DEMO_USER orders=3"

step "6. verify CDC replication (topics carry the demo rows)"
sleep 12
TOPICS=$(curl -s -m 10 http://localhost:8082/topics)
echo "$TOPICS" | grep -q cloudshop.public.users || fail "users topic missing"
echo "$TOPICS" | grep -q cloudshop.public.orders || fail "orders topic missing"
echo "CDC_TOPICS_OK"

phase "VALIDATION"
step "7. compatibility"
api POST /v1/workloads/$WID/compatibility "$K-c01" '{"target_provider":"azure"}' >"$RUN_DIR/compat.json"
cat "$RUN_DIR/compat.json" | need "d['status']=='conditional'" -- "compat conditional"
step "8. migration plan"
api POST /v1/workloads/$WID/plan "$K-p01" '{"target_provider":"azure"}' >"$RUN_DIR/plan.json"
cat "$RUN_DIR/plan.json" | need "d['overall_status']=='conditional'" -- "plan conditional"
step "9. drift"
curl -s -m 15 -X POST "$CONTROL_PLANE_URL/v1/workloads/$WID/drift" -H "Idempotency-Key: $K-d01" \
  -H "Authorization: Bearer $CP_REQUESTER_TOKEN" \
  -H 'Content-Type: application/json' -d @"$ROOT/tests/fixtures/drift/clean.json" >"$RUN_DIR/drift.json"
cat "$RUN_DIR/drift.json" | need "d['drift_gate']=='clear'" -- "drift clear"
step "10. policy (readiness probe, expect approval_required on conditional)"
api POST /v1/workloads/$WID/readiness "$K-r01" '{"target_weight":0}' >"$RUN_DIR/policy.json"
cat "$RUN_DIR/policy.json" | need "d['decision']=='approval_required'" -- "policy gate"

step "11. approval (measure lag via rehearsal first)"
api POST /v1/migrations/$MID/rehearse "$K-h01-$INV" '{"environment":"dev","target_weight":0}' >"$RUN_DIR/lag-probe.json"
LAG=$(python3 -c "import json; print((json.load(open('$RUN_DIR/lag-probe.json')).get('cdc') or {}).get('cdc_lag_seconds') or 8)")
[ -n "$LAG" ] || fail "lag probe"
echo "LAG_PROBE=$LAG"
RT='{"target_weight":0,"environment":"dev","validation_status":"passed","cdc_lag_seconds":'$LAG',"target_healthy":true,"read_only_canary":true,"write_ownership":"aws"}'
APPROVAL_RESP=$(curl -s -m 15 -X POST "$CONTROL_PLANE_URL/v1/workloads/$WID/approvals" -H "Idempotency-Key: $K-a01" -H "Authorization: Bearer $CP_REQUESTER_TOKEN" -d "$RT")
AID=$(echo "$APPROVAL_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])" 2>/dev/null) || true
if [ -z "$AID" ]; then echo "APPROVAL_RESP: $APPROVAL_RESP"; fail "approve (create)"; fi
curl -s -m 15 -X POST "$CONTROL_PLANE_URL/v1/workloads/$WID/approvals/$AID/decision" -H "Idempotency-Key: $K-a02" -H "Authorization: Bearer $CP_APPROVER_TOKEN" -d '{"decision":"approved"}' | grep -q approved || fail "approve"
echo "APPROVAL=$AID lag=$LAG"

phase "REHEARSAL"
step "12. rehearsal -> REHEARSAL_READY"
# Approval binds the measured evidence (incl. cdc_lag_seconds); lag has
# ~1s quantization jitter between calls, so a correct APPROVAL_STALE is
# converged with nested bounded loops: the inner loop re-measures with the
# SAME approval (absorbs oscillation), the outer loop re-approves at the
# fresh measurement (adapts to drift). Every attempt needs an exact
# evidence match; the gate is never weakened; exhaustion fails loudly.
HSEQ=0; ATTEMPT=0
while [ "$ATTEMPT" -lt 2 ]; do
  ATTEMPT=$((ATTEMPT+1))
  for INNER in 1 2 3; do
    HSEQ=$((HSEQ+1))
    api POST /v1/migrations/$MID/rehearse "$K-hH$HSEQ-$INV" '{"environment":"dev","target_weight":0,"approval_id":"'$AID'"}' >"$RUN_DIR/rehearsal.json"
    if [ "$(python3 -c "import json; print(json.load(open('$RUN_DIR/rehearsal.json'))['status'])")" = "REHEARSAL_READY" ]; then break 2; fi
    LAG_R=$(python3 -c "import json; print((json.load(open('$RUN_DIR/rehearsal.json')).get('cdc') or {}).get('cdc_lag_seconds') or 8)")
    echo "rehearsal measure lag=$LAG_R attempt=$ATTEMPT.$INNER (approval lag bound, retrying)"
  done
  RT_R='{"target_weight":0,"environment":"dev","validation_status":"passed","cdc_lag_seconds":'$LAG_R',"target_healthy":true,"read_only_canary":true,"write_ownership":"aws"}'
  AID=$(curl -s -m 15 -X POST "$CONTROL_PLANE_URL/v1/workloads/$WID/approvals" -H "Idempotency-Key: $K-ar$ATTEMPT" -H "Authorization: Bearer $CP_REQUESTER_TOKEN" -d "$RT_R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
  curl -s -m 15 -X POST "$CONTROL_PLANE_URL/v1/workloads/$WID/approvals/$AID/decision" -H "Idempotency-Key: $K-ad$ATTEMPT" -H "Authorization: Bearer $CP_APPROVER_TOKEN" -d '{"decision":"approved"}' | grep -q approved || fail "approve (retry $ATTEMPT)"
  echo "APPROVAL_RETRY=$AID lag=$LAG_R attempt=$ATTEMPT"
done
cat "$RUN_DIR/rehearsal.json" | need "d['status']=='REHEARSAL_READY'" -- "rehearsal ready"

phase "CANARY"
step "13. canary 0-50 (stage 100 exists but is not exercised here; readiness gates on 1/5/25/50)"
BASE='{"error_rate_5xx":0.002,"p95_ms":200,"p99_ms":400,"requests":500,"window_secs":900,"healthy":true}'
canary() { api POST /v1/migrations/$MID/canary "$K-can$1" '{"stage":'$1',"expected_stage":'$2',"baseline":'$BASE',"observed":'$3'}' | python3 -c "import sys,json; d=json.load(sys.stdin); assert d['verdict']=='PASS', d; print('canary stage', d['stage'], d['verdict'])"; }
canary 0 0 '{"error_rate_5xx":0.002,"p95_ms":200,"p99_ms":400,"requests":500,"window_secs":900,"healthy":true}'
canary 1 1 '{"error_rate_5xx":0.003,"p95_ms":210,"p99_ms":420,"requests":150,"window_secs":300,"healthy":true}'
canary 5 5 '{"error_rate_5xx":0.004,"p95_ms":220,"p99_ms":430,"requests":400,"window_secs":900,"healthy":true}'
canary 25 25 '{"error_rate_5xx":0.004,"p95_ms":230,"p99_ms":440,"requests":400,"window_secs":1800,"healthy":true}'
canary 50 50 '{"error_rate_5xx":0.005,"p95_ms":240,"p99_ms":450,"requests":400,"window_secs":3600,"healthy":true}'

phase "CUTOVER"
step "14. quiesce writes"
api POST /v1/migrations/$MID/quiesce "$K-q01" '{"quiesced":true}' | grep -q '"quiesced":true' || fail "quiesce"
curl -s -m 10 -o /dev/null -w '%{http_code}' -X POST "$SHOP_SRC_URL/v1/orders" -H "Idempotency-Key: $K-oq" \
  -d '{"user_id":"'$DEMO_USER'","total_cents":1}' | grep -q 503 || fail "quiesce must 503"

step "14b. CDC catch-up: drain pending traffic records to target"
# Rehearsal/readiness/cutover replication only carries probe records; demo
# traffic (users + orders) is replicated here through the same canonical
# path (FromDebezium + PostgresStore.ApplyAtomically, idempotent). Runs
# after quiesce so no new source writes can arrive mid-drain. Metrics are
# measured and printed, never asserted into existence.
REDPANDA_PROXY_URL=http://localhost:8082 \
CDC_TARGET_DATABASE_URL="postgres://cloudshop:cloudshop@localhost:5434/cloudshop?sslmode=disable" \
  "$RUN_DIR/cdc-catchup" >"$RUN_DIR/catchup.log" 2>&1 || fail "cdc catch-up"
cat "$RUN_DIR/catchup.log"
grep -q CATCHUP_OK "$RUN_DIR/catchup.log" || fail "cdc catch-up"

step "15-16. CDC catch-up + final validation via readiness"
# Same nested convergence loop as step 12 (inner: same approval absorbs
# jitter; outer: re-approval adapts to drift; exhaustion fails loudly).
RD_ATTEMPT=0; RD_SEQ=0
while [ "$RD_ATTEMPT" -lt 2 ]; do
  RD_ATTEMPT=$((RD_ATTEMPT+1))
  for RD_INNER in 1 2 3; do
    RD_SEQ=$((RD_SEQ+1))
    api POST /v1/migrations/$MID/readiness "$K-rdS$RD_SEQ" '{"environment":"dev","approval_id":"'$AID'"}' >"$RUN_DIR/readiness.json" || true
    LAG2=$(python3 -c "import json; d=json.load(open('$RUN_DIR/readiness.json')); print((d.get('cdc') or {}).get('cdc_lag_seconds'))")
    STATUS=$(python3 -c "import json; print(json.load(open('$RUN_DIR/readiness.json'))['status'])")
    echo "readiness=$STATUS lag=$LAG2 attempt=$RD_ATTEMPT.$RD_INNER"
    if [ "$STATUS" = "READY_FOR_CUTOVER" ]; then break 2; fi
  done
  RT2='{"target_weight":0,"environment":"dev","validation_status":"passed","cdc_lag_seconds":'$LAG2',"target_healthy":true,"read_only_canary":true,"write_ownership":"aws"}'
  AID=$(curl -s -m 15 -X POST "$CONTROL_PLANE_URL/v1/workloads/$WID/approvals" -H "Idempotency-Key: $K-a1$RD_ATTEMPT" -H "Authorization: Bearer $CP_REQUESTER_TOKEN" -d "$RT2" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
  curl -s -m 15 -X POST "$CONTROL_PLANE_URL/v1/workloads/$WID/approvals/$AID/decision" -H "Idempotency-Key: $K-a2$RD_ATTEMPT" -H "Authorization: Bearer $CP_APPROVER_TOKEN" -d '{"decision":"approved"}' >/dev/null
done
cat "$RUN_DIR/readiness.json" | need "d['status']=='READY_FOR_CUTOVER'" -- "readiness ready"

step "17. final cutover -> CUTOVER_COMPLETE"
# Cutover re-measures CDC at execution, so lag can jitter again between the
# readiness approval and the transfer: a correct APPROVAL_STALE blocks the
# attempt with no mutation (preflight gate first). Converge with the same
# nested pattern — fresh lag probe (migration readiness without approval
# still returns its CDC measurement), re-approve, retry with a fresh
# idempotency key per attempt (same key + new approval would 409).
# Retrying a pre-transfer block is safe; a partial flip would resume via
# the designed resume path. Exhaustion fails loudly.
FATTEMPT=0
while [ "$FATTEMPT" -lt 3 ]; do
  FATTEMPT=$((FATTEMPT+1))
  api POST /v1/migrations/$MID/cutover/final "$K-f0$FATTEMPT" '{"environment":"dev","approval_id":"'$AID'"}' >"$RUN_DIR/cutover.json"
  if [ "$(python3 -c "import json; print(json.load(open('$RUN_DIR/cutover.json'))['status'])")" = "CUTOVER_COMPLETE" ]; then break; fi
  echo "cutover attempt $FATTEMPT blocked: $(python3 -c "import json; d=json.load(open('$RUN_DIR/cutover.json')); print(d.get('failure_code'), '|', d.get('failure_reason'))")"
  LAG_F=$(api POST /v1/migrations/$MID/readiness "$K-fp$FATTEMPT" '{"environment":"dev"}' | python3 -c "import sys,json; print((json.load(sys.stdin).get('cdc') or {}).get('cdc_lag_seconds') or 8)")
  RT_F='{"target_weight":0,"environment":"dev","validation_status":"passed","cdc_lag_seconds":'$LAG_F',"target_healthy":true,"read_only_canary":true,"write_ownership":"aws"}'
  AID=$(curl -s -m 15 -X POST "$CONTROL_PLANE_URL/v1/workloads/$WID/approvals" -H "Idempotency-Key: $K-fa$FATTEMPT" -H "Authorization: Bearer $CP_REQUESTER_TOKEN" -d "$RT_F" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
  curl -s -m 15 -X POST "$CONTROL_PLANE_URL/v1/workloads/$WID/approvals/$AID/decision" -H "Idempotency-Key: $K-fd$FATTEMPT" -H "Authorization: Bearer $CP_APPROVER_TOKEN" -d '{"decision":"approved"}' | grep -q approved || fail "approve (cutover retry $FATTEMPT)"
  echo "APPROVAL_CUTOVER_RETRY=$AID lag=$LAG_F attempt=$FATTEMPT"
done
cat "$RUN_DIR/cutover.json" | need "d['status']=='CUTOVER_COMPLETE'" -- "cutover complete"
cat "$RUN_DIR/cutover.json" | need "d['current_owner']=='azure'" -- "azure authoritative"

phase "POST-CUTOVER"
step "18. Azure writes resume, AWS rejected, reconciliation"
TGT_USER=$(psql_tgt "SELECT id FROM users WHERE email='demo-$RUN_ID@example.invalid';" | tr -d ' \n\r\t')
[ -z "$TGT_USER" ] && fail "demo user not replicated"
curl -s -m 10 -X POST "$SHOP_TGT_URL/v1/orders" -H "Idempotency-Key: $K-ot" \
  -d '{"user_id":"'$TGT_USER'","total_cents":4242}' | python3 -c "import sys,json; assert json.load(sys.stdin)['status']=='pending'" 2>/dev/null || fail "azure write"
curl -s -m 10 -o /dev/null -w '%{http_code}' -X POST "$SHOP_SRC_URL/v1/orders" -H "Idempotency-Key: $K-oa" \
  -d '{"user_id":"'$DEMO_USER'","total_cents":1}' | grep -q 403 || fail "aws must reject"
echo "AZURE_WRITE_OK AWS_REJECTED_OK replicated_user=$TGT_USER"

phase "EVIDENCE"
step "19-20. final evidence"
SRC_OWN=$(curl -s -m 8 -H "Authorization: Bearer $SHOP_ADMIN_TOKEN" "$SHOP_SRC_URL/v1/admin/ownership" | python3 -c "import sys,json; print(json.load(sys.stdin)['write_ownership'])")
TGT_OWN=$(curl -s -m 8 -H "Authorization: Bearer $SHOP_ADMIN_TOKEN" "$SHOP_TGT_URL/v1/admin/ownership" | python3 -c "import sys,json; print(json.load(sys.stdin)['write_ownership'])")
[ "$SRC_OWN" = "azure" ] && [ "$TGT_OWN" = "azure" ] || fail "ownership views"
[ "$SRC_OWN" = "$TGT_OWN" ] || fail "split brain"
python3 -c "
import json
d = json.load(open('$RUN_DIR/cutover.json'))
assert d['routing'] == 'azure', d
assert d['reconciliation']['match'] is True, d['reconciliation']
print('lag:', d['cdc_lag_seconds'], '| src:', d['source_lsn'], '| applied:', d['applied_lsn'])
print('stages:', [s['name'] for s in d['stages']])"
echo "WID=$WID MID=$MID AID=$AID RUN=$RUN_ID" >"$RUN_DIR/demo-ids.env"
echo "DEMO_MIGRATION = COMPLETE (azure authoritative, aws rejects, no split brain)"
# Final summary: every line below restates a gate already asserted above
# (reaching this point means all of them passed); lag/reconciliation are
# re-read from the cutover evidence file, never hardcoded.
SUMMARY_LAG=$(python3 -c "import json; print(json.load(open('$RUN_DIR/cutover.json'))['cdc_lag_seconds'])")
SUMMARY_RECON=$(python3 -c "import json; print('YES' if json.load(open('$RUN_DIR/cutover.json'))['reconciliation']['match'] is True else 'NO')")
echo ""
echo "SOURCE        AWS"
echo "TARGET        AZURE"
echo "AUTHORITY     AZURE"
echo "CDC LAG       ${SUMMARY_LAG}s"
echo "RECONCILED    $SUMMARY_RECON"
echo "CUTOVER       COMPLETE"
echo "AZURE WRITE   PASS"
echo "AWS WRITE     REJECTED"
echo "SPLIT BRAIN   PREVENTED"
