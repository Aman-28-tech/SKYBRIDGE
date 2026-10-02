#!/usr/bin/env bash
# SKYBRIDGE portfolio demo: presentation wrapper around the proven pipeline.
#
#   1. local-only safety gate (read-only; mirrors showcase.sh)
#   2. doctor            (lab health, read-only)
#   3. reset             (AWS authoritative, Azure standby)
#   4. lab up (idempotent; starts services only if needed)
#   5. connector ensured (registers only if missing)
#   6. demo-migration    (full AWS->Azure lifecycle, CUTOVER_COMPLETE)
#   7. status            (current-state summary)
#   8. final summary + console pointers
#
# Unlike showcase.sh this wrapper does NOT run the failure matrix and does
# NOT reset afterwards: services and evidence stay up so the console
# (http://localhost:3000/demo) can observe the completed migration.
#
# Local infrastructure only. Zero AWS calls, zero Azure calls, $0 spend.
# No Terraform apply/destroy, no cloud credentials, no mutation-boundary
# bypass: every step below is a pre-existing read-only/local script.
# Final line: SKYBRIDGE_PORTFOLIO_DEMO = PASS. Exit code matches verdict.
#
# Env (all optional): DEMO_RUN_ID, CP_PORT/SRC_PORT/TGT_PORT, COMPOSE.
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export DEMO_RUN_ID="${DEMO_RUN_ID:-$(date +%s)}"
export CP_PORT="${CP_PORT:-18080}" SRC_PORT="${SRC_PORT:-8085}" TGT_PORT="${TGT_PORT:-8086}"
export COMPOSE="${COMPOSE:-docker compose}"
OVERALL=0
echo "PORTFOLIO_DEMO_RUN_ID=$DEMO_RUN_ID"

step() { # name, command...
  local name="$1"; shift
  echo "--- portfolio-demo: $name ---"
  if "$@"; then echo "PORTFOLIO_STEP $name = PASS"; else echo "PORTFOLIO_STEP $name = FAIL"; OVERALL=1; fi
}

gate() { # read-only local-only boundary check (mirrors showcase.sh)
  cd "$ROOT" || return 1
  ! grep -rn 'aws-sdk-go\|azure-sdk\|management.azure.com\|sts.amazonaws.com' apps/ workloads/ --include='*.go' | grep -v _test | grep -q . || return 1
  ! grep -rnE 'terraform (apply|destroy)' scripts/*.sh | grep -v 'NEVER\|never runs\|MUST NEVER' | grep -vE ':[0-9]+:[[:space:]]*#' | grep -q . || return 1
  grep -q 'ApplyEnabled = false' apps/control-plane/tfvars.go || return 1
  echo "LOCAL_ONLY_BOUNDARY_OK (no cloud SDKs, no apply/destroy, ApplyEnabled=false)"
}

lab_up() { # idempotent: start lab services if needed, then ensure connector
  $COMPOSE -f "$ROOT/docker-compose.yml" up -d || return 1
  $COMPOSE -f "$ROOT/docker-compose.yml" --profile cdc up -d || return 1
  for i in $(seq 1 30); do
    curl -sf -m 10 http://localhost:8083/connectors >/dev/null 2>&1 && break
    sleep 5
    [ "$i" = 30 ] && return 1
  done
  if curl -sf -m 10 http://localhost:8083/connectors/cloudshop-pg-connector >/dev/null 2>&1; then
    echo "CONNECTOR_PRESENT (already registered)"
  else
    "$ROOT/scripts/cdc-register.sh" || return 1
  fi
}

step CLOUD_GATE gate
if [ "$OVERALL" -ne 0 ]; then echo "SKYBRIDGE_PORTFOLIO_DEMO = FAIL"; exit 1; fi
step DOCTOR "$ROOT/scripts/doctor.sh"
step RESET_BEFORE "$ROOT/scripts/reset-demo.sh"
step LAB_UP lab_up
step DEMO_MIGRATION "$ROOT/scripts/demo-migration.sh"
step STATUS "$ROOT/scripts/skybridge-status.sh"

if [ "$OVERALL" -eq 0 ]; then
  # shellcheck disable=SC1090
  . "$ROOT/.demo-run/demo-ids.env" 2>/dev/null || true
  echo ""
  echo "SOURCE        AWS"
  echo "TARGET        AZURE"
  echo "AUTHORITY     AZURE"
  echo "CUTOVER       COMPLETE"
  echo "CLOUD_SPEND   \$0 (local only, no AWS/Azure calls)"
  echo ""
  echo "Observe the result (services left running with evidence intact):"
  echo "  cd apps/console && npm run dev   # then open http://localhost:3000/demo"
  echo "  ./scripts/skybridge-status.sh     # read-only state summary"
  echo ""
  echo "SKYBRIDGE_PORTFOLIO_DEMO = PASS"
else
  echo "SKYBRIDGE_PORTFOLIO_DEMO = FAIL"
fi
exit "$OVERALL"
