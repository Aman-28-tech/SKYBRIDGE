#!/usr/bin/env bash
# Register the Debezium Postgres connector (requires the `cdc` compose profile up).
# Usage: docker compose --profile cdc up -d && ./scripts/cdc-register.sh
set -euo pipefail
CONNECT_URL="${CONNECT_URL:-http://localhost:8083}"
echo "Waiting for Kafka Connect at ${CONNECT_URL}..."
for i in $(seq 1 30); do
  if curl -sf -m 10 "${CONNECT_URL}/connectors" >/dev/null; then break; fi
  sleep 5
done
curl -sf -m 10 -X POST "${CONNECT_URL}/connectors" \
  -H 'Content-Type: application/json' \
  -d @debezium/postgres-connector.json && echo && echo "CONNECTOR REGISTERED"
curl -sf -m 10 "${CONNECT_URL}/connectors/cloudshop-pg-connector/status"
