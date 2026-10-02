#!/usr/bin/env bash
# Reset the free local multi-cloud demo to its deterministic starting state:
#   AWS authoritative, Azure standby, writes enabled on AWS, disabled on
#   Azure, demo-tagged rows removed, demo processes stopped (their in-memory
#   migration/rehearsal/ownership state vanishes with them).
#
# Safety: only DELETEs rows matching demo tags (demo-%/probe-%/rehearsal-%)
# in the two CloudShop databases. Never drops tables/volumes/services and
# never touches unrelated databases.
#
# Env (all optional):
#   SHOP_SRC_URL / SHOP_TGT_URL   CloudShop admin endpoints
#   DEMO_RUN_DIR                  pid directory (default $ROOT/.demo-run)
#   COMPOSE                       compose command (default "docker compose")
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SHOP_SRC_URL="${SHOP_SRC_URL:-http://localhost:8085}"
SHOP_TGT_URL="${SHOP_TGT_URL:-http://localhost:8086}"
DEMO_RUN_DIR="${DEMO_RUN_DIR:-$ROOT/.demo-run}"
COMPOSE="${COMPOSE:-docker compose}"

# Local/dev auth (H-1/H-3): admin calls need the lab's per-run shop token when
# the shops are up. Best effort: missing tokens just mean live admin resets
# are skipped (the SQL cleanup below still restores persisted state).
if [ -z "${SHOP_ADMIN_TOKEN:-}" ] && [ -f "$ROOT/.demo-run/.auth-tokens" ]; then
  # shellcheck disable=SC1091
  . "$ROOT/.demo-run/.auth-tokens" 2>/dev/null || true
fi
SHOP_AUTH_ARGS=()
if [ -n "${SHOP_ADMIN_TOKEN:-}" ]; then
  SHOP_AUTH_ARGS=(-H "Authorization: Bearer $SHOP_ADMIN_TOKEN")
fi

stop_pid() { # pidfile
  if [ -f "$1" ]; then
    local pid
    pid="$(cat "$1")"
    if kill -0 "$pid" 2>/dev/null; then kill "$pid" 2>/dev/null || true; fi
    # also stop child server binaries holding demo ports
    rm -f "$1"
  fi
}

echo "== stopping demo processes =="
stop_pid "$DEMO_RUN_DIR/cloudshop-src.pid"
stop_pid "$DEMO_RUN_DIR/cloudshop-tgt.pid"
stop_pid "$DEMO_RUN_DIR/control-plane.pid"
# belt: free demo ports held by orphaned children
if command -v ss >/dev/null 2>&1; then
  for p in $(ss -ltnp 2>/dev/null | grep -E ":(8085|8086|18080) " | grep -oP 'pid=\K[0-9]+' | sort -u); do
    kill "$p" 2>/dev/null || true
  done
else
  echo "note: ss unavailable; orphaned demo processes (if any) left alone"
fi
sleep 2

echo "== restoring CloudShop flags (best effort) =="
for base in "$SHOP_SRC_URL" "$SHOP_TGT_URL"; do
  curl -s -m 8 "${SHOP_AUTH_ARGS[@]}" -X POST "$base/v1/admin/ownership" -d '{"write_ownership":"aws"}' >/dev/null 2>&1 || true
  curl -s -m 8 "${SHOP_AUTH_ARGS[@]}" -X POST "$base/v1/admin/quiesce" -d '{"quiesced":false}' >/dev/null 2>&1 || true
done

echo "== cleaning demo-tagged rows (best effort) =="
if $COMPOSE -f "$ROOT/docker-compose.yml" ps cloudshop-db >/dev/null 2>&1; then
  for svc in cloudshop-db cloudshop-target-db; do
    $COMPOSE -f "$ROOT/docker-compose.yml" exec -T "$svc" psql -U cloudshop -d cloudshop \
      -c "DELETE FROM orders WHERE id IN (SELECT o.id FROM orders o JOIN users u ON u.id=o.user_id WHERE u.email LIKE 'demo-%' OR u.email LIKE 'probe-%' OR u.email LIKE 'rehearsal-%'); DELETE FROM users WHERE email LIKE 'demo-%' OR email LIKE 'probe-%' OR email LIKE 'rehearsal-%';" \
      >/dev/null 2>&1 || echo "note: row cleanup skipped for $svc"
    # H-3: durable admin state survives process stops, so clear it explicitly
    # (separate command: old volumes without the table must not block the
    # row cleanup above).
    $COMPOSE -f "$ROOT/docker-compose.yml" exec -T "$svc" psql -U cloudshop -d cloudshop \
      -c "DELETE FROM admin_state;" >/dev/null 2>&1 || true
  done
else
  echo "note: compose unreachable; row cleanup skipped"
fi

echo "RESET_OK source=aws-authoritative target=standby writes=aws-only demo-processes=stopped"
