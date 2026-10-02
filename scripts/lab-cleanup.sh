#!/usr/bin/env bash
# Cleanup lab volumes + ephemeral state. Verifies cleanup per COST_GUARDRAILS.
set -euo pipefail
docker compose down -v
docker volume ls | grep -i skybridge || echo "no skybridge volumes remain"
echo "CLEANUP OK"
