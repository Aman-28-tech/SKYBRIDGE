# Runbook — Migration Rehearsal

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-25

## Purpose

Practice the full migration on an ephemeral target without touching production traffic.

## Preconditions

- [ ] Compat `overall == pass` (report ID recorded); `conditional` needs approval ID
- [ ] Zero open `blocking`/`security_critical` drift
- [ ] Target provisioned from pinned Terraform modules; CloudShop `readyz` 200 with correct `schema_version`
- [ ] CDC running; `cdc_lag_seconds` observable

## Commands (placeholders)

```bash
# create rehearsal run (Idempotency-Key required)
curl -X POST /api/v1/migrations -H 'Idempotency-Key: <uuid>' -d '{"workload_id":"<uuid>","target_provider":"azure"}'
curl -X POST /api/v1/migrations/<id>/rehearsal -H 'Idempotency-Key: <uuid>'
curl -X POST /api/v1/migrations/<id>/validation -H 'Idempotency-Key: <uuid>'
# synthetic load (k6 profile from tests/load/)
k6 run tests/load/canary-baseline.js
```

## Verification

- [ ] Row counts + checksums source vs target match; FKs valid; manifest (key+hash) reconciled
- [ ] Queue drain gate (depth <= 10, 60s sustained) demonstrated
- [ ] RPO gate (`cdc_lag_seconds <= 30` sustained 60s) + RTO measured (trigger → valid traffic + health + validation)
- [ ] Audit complete (intent + completion per mutation, evidence IDs)

## Failure symptoms

- Lag never catches up → check Debezium connector, broker retention, applier errors, clock skew
- Validation fails → see Validation Analyst output with evidence IDs; do not advance

## Rollback/recovery

- Rehearsal uses ephemeral target; rollback = delete ephemeral scope + record lesson. Never route production traffic during rehearsal.

## Cleanup

- [ ] Ephemeral target destroyed + verified; manifest of deleted scopes retained
- [ ] Actual spend recorded; billing alerts intact

## Owner

Migration operator + approver (for conditional compat).
