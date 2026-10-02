# Migration Rehearsal — Local Slice

> Status: implemented, proven locally 2026-09-27.
> Endpoint: `POST /v1/migrations/{id}/rehearse` (`apps/control-plane/rehearsal.go`).
> Data plane: `apps/control-plane/rehearsal_cdc.go` (live PG + Pandaproxy,
> stdlib + lib/pq). CDC core imported from `skybridge/cdc-applier`
> (refactored to `package cdc`; `cmd/cdc-applier` holds the binary;
> semantics unchanged, 37/37 CDC tests still green).

## What it is

A deterministic dry run over the completed slices — workload → compatibility
→ plan → drift → policy → approval → provisioning intent → CDC replication →
validation → readiness — ending at `REHEARSAL_READY`. It creates no migration
resources, no Temporal executions, no workflows. Persistence: strict-semantic
idempotency record + audit events only (existing systems).

## Stage machine

`accepted → evidence → provisional_policy → provisioning_tfvars →
replication → reconciliation → final_policy → provisioning_intent →
REHEARSAL_READY` (first failure stops later stages; `REHEARSAL_BLOCKED`
carries `failure_code`/`failure_reason`). HTTP 200 carries READY and BLOCKED
(a block is a decision, like readiness); 4xx only for malformed/stale/
conflict input. `Apply` is never called (no code path; `ApplyEnabled=false`).

Evidence freshness reuses `buildPolicyInput` refusal codes
(`COMPATIBILITY_STALE`, `PLAN_STALE`, `NO_DRIFT_EVIDENCE`); provisional
policy enforces measurement-independent denies (compat block, drift,
canonical weight); final policy evaluates MEASURED lag/health/validation.
Approval follows existing binding exactly (`approvalEligibleForUse`); the
conditional path requires an approval bound to the measured runtime
(operator loop: rehearse → read measured lag → approve → decide → rehearse
with `approval_id`).

## Topics/ordering/lag

Inherited from the CDC slice: Debezium-default topics, LSN ordering
authority, per-event durable dedupe (`cdc_applied_events`), forward-only
checkpoints, `cdc_lag_seconds = observe − commit`, RPO 30s from measurement.
Reconciliation is scoped to the rehearsal probe set (deterministic UUIDs from
the idempotency key); counts are context; IDs/field names only, never values.

## Proven locally (2026-09-27)

`REHEARSAL_READY` with `lag=2s`, `within_rpo`, `recon match=true`,
`captured=2 applied=2 duplicates=1` (replay proof), adapter + tfvars
fingerprints, zero executions, `lifecycle_state=registered`. Full command
sequence: see acceptance transcript (register → migration → compat →
plan → drift clean → rehearse → approve → decide → rehearse).

## Deferred (not claimed)

Production RPO/RTO (rehearsal timings only: replication ~2.1s,
reconciliation ~65ms, total ~2.2s), cross-transaction atomicity, production
provisioning, traffic switching, cutover, rollback, reverse CDC.
