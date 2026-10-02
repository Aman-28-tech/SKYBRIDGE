#!/usr/bin/env bash
# SKYBRIDGE doctor: read-only health check of the local demo lab.
# Prints CHECK_NAME = PASS/FAIL per check. No mutation (no up/down,
# no restarts, no writes). Exit 0 only if every check passes.
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMPOSE="${COMPOSE:-docker compose}"
export COMPOSE ROOT
OVERALL=0

check() { # name, command...
  local name="$1"; shift
  if "$@" >/dev/null 2>&1; then echo "$name = PASS"; else echo "$name = FAIL"; OVERALL=1; fi
}

compose_exec() { # service, command...
  local svc="$1"; shift
  $COMPOSE -f "$ROOT/docker-compose.yml" exec -T "$svc" "$@" 2>/dev/null
}

check Docker docker info
check PostgreSQL compose_exec postgres pg_isready -U skybridge
check CloudShop_DB compose_exec cloudshop-db pg_isready -U cloudshop
check Target_PostgreSQL compose_exec cloudshop-target-db pg_isready -U cloudshop
check Redis compose_exec redis redis-cli ping
check Redpanda bash -c 'out=$(curl -sf -m 8 http://localhost:9644/v1/status/ready) && echo "$out" | grep -q "\"status\"[[:space:]]*:[[:space:]]*\"ready\""'
check Azurite bash -c '$COMPOSE -f "$ROOT/docker-compose.yml" ps azurite 2>/dev/null | grep -qE "Up|running"'
check Temporal python3 -c "import socket; s=socket.create_connection(('localhost',7233),timeout=8); s.close()"
check Go go version
check Python python3 --version
check Terraform bash -c 'TF_BIN="${TERRAFORM_BIN:-terraform}"; command -v "$TF_BIN" >/dev/null && "$TF_BIN" version >/dev/null'

if [ "$OVERALL" -eq 0 ]; then echo "DOCTOR = PASS"; else echo "DOCTOR = FAIL"; fi
exit "$OVERALL"
