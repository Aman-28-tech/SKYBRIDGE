#!/usr/bin/env bash
# SKYBRIDGE demo status: compact current-state summary of the free local
# multi-cloud lab (cloudshop-local-aws-to-azure).
#
# Read-only: queries live admin/health endpoints and reads the demo-run
# artifacts written by demo-migration.sh. Never mutates, never prints
# secrets (IDs, positions, hashes and flags only).
#
# Env (all optional):
#   CP_PORT/SRC_PORT/TGT_PORT (defaults 18080/8085/8086)
#   CONTROL_PLANE_URL/SHOP_SRC_URL/SHOP_TGT_URL (derived from ports)
#   COMPOSE (default "docker compose")
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CP_PORT="${CP_PORT:-18080}"
SRC_PORT="${SRC_PORT:-8085}"
TGT_PORT="${TGT_PORT:-8086}"
CONTROL_PLANE_URL="${CONTROL_PLANE_URL:-http://localhost:$CP_PORT}"
SHOP_SRC_URL="${SHOP_SRC_URL:-http://localhost:$SRC_PORT}"
SHOP_TGT_URL="${SHOP_TGT_URL:-http://localhost:$TGT_PORT}"
COMPOSE="${COMPOSE:-docker compose}"
RUN_DIR="$ROOT/.demo-run"

live() { curl -s -m 5 "$1" 2>/dev/null || true; }
# Authenticated admin GET (H-3): the admin surface requires a bearer token.
# Uses this lab's per-run token when available; without it the shops report
# UNKNOWN (never an error, never a secret printed).
SHOP_AUTH_ARGS=()
if [ -f "$RUN_DIR/.auth-tokens" ]; then
  # shellcheck disable=SC1091
  . "$RUN_DIR/.auth-tokens" 2>/dev/null || true
  if [ -n "${SHOP_ADMIN_TOKEN:-}" ]; then
    SHOP_AUTH_ARGS=(-H "Authorization: Bearer $SHOP_ADMIN_TOKEN")
  fi
fi
live_admin() { curl -s -m 5 "${SHOP_AUTH_ARGS[@]}" "$1" 2>/dev/null || true; }
# jget <file> <python-expr> : print expr over parsed JSON, or UNKNOWN.
jget() {
  python3 -c "
import sys, json
try:
    d = json.load(open('$1'))
    print(($2) if ($2) is not None else 'UNKNOWN')
except Exception:
    print('UNKNOWN')" 2>/dev/null
}

SRC_OWN=$(live_admin "$SHOP_SRC_URL/v1/admin/ownership" | python3 -c "import sys,json; print(json.load(sys.stdin).get('write_ownership','UNKNOWN'))" 2>/dev/null || echo UNKNOWN)
TGT_OWN=$(live_admin "$SHOP_TGT_URL/v1/admin/ownership" | python3 -c "import sys,json; print(json.load(sys.stdin).get('write_ownership','UNKNOWN'))" 2>/dev/null || echo UNKNOWN)
SRC_QUIESCE=$(live_admin "$SHOP_SRC_URL/v1/admin/quiesce" | python3 -c "import sys,json; print(json.load(sys.stdin).get('quiesced','UNKNOWN'))" 2>/dev/null || echo UNKNOWN)

if [ "$SRC_OWN" = "UNKNOWN" ] && [ "$TGT_OWN" = "UNKNOWN" ]; then AUTHORITY="UNKNOWN (shops unreachable)"
elif [ "$SRC_OWN" = "$TGT_OWN" ]; then AUTHORITY="$SRC_OWN"
else AUTHORITY="MIXED src=$SRC_OWN tgt=$TGT_OWN"; fi

if [ "$SRC_OWN" = "UNKNOWN" ] || [ "$TGT_OWN" = "UNKNOWN" ]; then SPLIT="UNKNOWN (shops unreachable)"
elif [ "$SRC_OWN" = "$TGT_OWN" ]; then SPLIT="AGREE ($SRC_OWN: no split brain)"
else SPLIT="DISAGREE src=$SRC_OWN tgt=$TGT_OWN: SPLIT-BRAIN RISK"; fi

CUTOVER_JSON="$RUN_DIR/cutover.json"
READINESS_JSON="$RUN_DIR/readiness.json"
CUTOVER_STATE="NOT_STARTED"
if [ -f "$CUTOVER_JSON" ]; then CUTOVER_STATE=$(jget "$CUTOVER_JSON" "d.get('status')"); fi
LIFECYCLE="$CUTOVER_STATE"
if [ "$LIFECYCLE" = "NOT_STARTED" ] && [ -f "$READINESS_JSON" ]; then
  LIFECYCLE=$(jget "$READINESS_JSON" "d.get('status')")
fi
ROUTING="UNKNOWN"
if [ -f "$CUTOVER_JSON" ]; then ROUTING=$(jget "$CUTOVER_JSON" "d.get('routing')"); fi

WID="UNKNOWN"; MID="UNKNOWN"; AID="NONE"
if [ -f "$RUN_DIR/demo-ids.env" ]; then
  # shellcheck disable=SC1090
  . "$RUN_DIR/demo-ids.env" 2>/dev/null || true
  WID="${WID:-UNKNOWN}"; MID="${MID:-UNKNOWN}"; AID="${AID:-NONE}"
fi

SRC_POS="UNKNOWN"; APPLIED_POS="UNKNOWN"; LAG="UNKNOWN"
for f in "$CUTOVER_JSON" "$READINESS_JSON" "$RUN_DIR/lag-probe.json"; do
  if [ -f "$f" ]; then
    P=$(jget "$f" "(d.get('cdc') or {}).get('source_lsn') or d.get('source_lsn')")
    A=$(jget "$f" "(d.get('cdc') or {}).get('applied_lsn') or d.get('applied_lsn')")
    L=$(jget "$f" "(d.get('cdc') or {}).get('cdc_lag_seconds', d.get('cdc_lag_seconds'))")
    [ "$SRC_POS" = "UNKNOWN" ] && [ "$P" != "UNKNOWN" ] && SRC_POS="$P"
    [ "$APPLIED_POS" = "UNKNOWN" ] && [ "$A" != "UNKNOWN" ] && APPLIED_POS="$A"
    [ "$LAG" = "UNKNOWN" ] && [ "$L" != "UNKNOWN" ] && LAG="${L}s"
  fi
done
CAPTURED="UNKNOWN"; APPLIED_EV="UNKNOWN"; DUPS="UNKNOWN"
if [ -f "$RUN_DIR/catchup.log" ]; then
  LINE=$(grep -h CATCHUP_OK "$RUN_DIR/catchup.log" 2>/dev/null | tail -n 1)
  CAPTURED=$(echo "$LINE" | python3 -c "import sys,re; m=re.search(r'captured=(\d+)',sys.stdin.read()); print(m.group(1) if m else 'UNKNOWN')" 2>/dev/null)
  APPLIED_EV=$(echo "$LINE" | python3 -c "import sys,re; m=re.search(r'applied=(\d+)',sys.stdin.read()); print(m.group(1) if m else 'UNKNOWN')" 2>/dev/null)
  DUPS=$(echo "$LINE" | python3 -c "import sys,re; m=re.search(r'duplicates=(\d+)',sys.stdin.read()); print(m.group(1) if m else 'UNKNOWN')" 2>/dev/null)
fi

POLICY="UNKNOWN"
[ -f "$RUN_DIR/policy.json" ] && POLICY=$(jget "$RUN_DIR/policy.json" "d.get('decision')")
CANARY="UNKNOWN"; QUIESCE_STATE="UNKNOWN"; RECON="UNKNOWN"
for f in "$CUTOVER_JSON" "$READINESS_JSON"; do
  [ -f "$f" ] || continue
  for check in canary quiesce reconciliation; do
    VAL=$(python3 -c "
import json
try:
    d = json.load(open('$f'))
    for c in (d.get('readiness') or {}).get('checks', []):
        if c.get('name') == '$check':
            print(('PASS' if c.get('pass') else 'FAIL') + ' (' + str(c.get('detail',''))[:60] + ')')
            break
except Exception:
    pass" 2>/dev/null)
    case "$check" in
      canary) [ "$CANARY" = "UNKNOWN" ] && [ -n "$VAL" ] && CANARY="$VAL" ;;
      quiesce) [ "$QUIESCE_STATE" = "UNKNOWN" ] && [ -n "$VAL" ] && QUIESCE_STATE="$VAL" ;;
    esac
  done
done
if [ -f "$CUTOVER_JSON" ]; then
  RECON=$(python3 -c "
import json
try:
    d = json.load(open('$CUTOVER_JSON'))
    print('MATCH' if (d.get('reconciliation') or {}).get('match') is True else 'MISMATCH')
except Exception:
    print('UNKNOWN')" 2>/dev/null)
fi

CLOUD_AWS="DISABLED"; CLOUD_AZURE="DISABLED"
grep -q 'ApplyEnabled = false' "$ROOT/apps/control-plane/tfvars.go" 2>/dev/null || { CLOUD_AWS="CHECK_FAILED"; CLOUD_AZURE="CHECK_FAILED"; }
if env | cut -d= -f1 | grep -qE '^(AWS_|AZURE_)'; then
  CLOUD_AWS="ENV_KEYS_PRESENT (no calls made)"
  CLOUD_AZURE="ENV_KEYS_PRESENT (no calls made)"
fi

LOCAL_DEMO="DOWN"
if $COMPOSE -f "$ROOT/docker-compose.yml" ps cloudshop-db 2>/dev/null | grep -qE "Up|running" \
  && $COMPOSE -f "$ROOT/docker-compose.yml" ps cloudshop-target-db 2>/dev/null | grep -qE "Up|running" \
  && $COMPOSE -f "$ROOT/docker-compose.yml" ps redpanda 2>/dev/null | grep -qE "Up|running"; then
  LOCAL_DEMO="READY"
fi

echo "Architecture:"
echo "  source provider   aws"
echo "  target provider   azure"
echo "  authority         $AUTHORITY"
echo "  routing           $ROUTING"
echo "Migration:"
echo "  workload          $WID"
echo "  migration         $MID"
echo "  lifecycle state   $LIFECYCLE"
echo "  cutover state     $CUTOVER_STATE"
echo "CDC:"
echo "  source position   $SRC_POS"
echo "  applied position  $APPLIED_POS"
echo "  lag               $LAG"
echo "  captured          $CAPTURED"
echo "  applied           $APPLIED_EV"
echo "  duplicates        $DUPS"
echo "Safety:"
echo "  policy            $POLICY"
echo "  approval          $AID"
echo "  canary            $CANARY"
echo "  quiesce           src_quiesced=$SRC_QUIESCE preflight=$QUIESCE_STATE"
echo "  reconciliation    $RECON"
echo "  split-brain       $SPLIT"
echo "Cloud:"
echo "  real AWS          $CLOUD_AWS/BLOCKED"
echo "  real Azure        $CLOUD_AZURE/BLOCKED"
echo "  local demo        $LOCAL_DEMO"
