# Migration Engine

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 2
> Last updated: 2026-09-25

## Pipeline (maps to WORKFLOW_STATE_MACHINE.md states)

```text
Discover (REGISTERED->DISCOVERED)
  -> Normalize (DISCOVERED->MODELED)
  -> Compatibility (MODELED->COMPATIBLE; overall pass|conditional|unknown|block)
  -> Plan (COMPATIBLE->PLANNED; MigrationPlan with rollback_plan + evidence_ids)
  -> Provision (PLANNED/APPROVED->PROVISIONING; Terraform apply per scope)
  -> Deploy (PROVISIONING->TARGET_READY; CloudShop target + readyz)
  -> Replicate (->REPLICATING; snapshot+CDC, cdc_lag_seconds tracked)
  -> Rehearse (->REHEARSING; ephemeral target, no production cutover)
  -> Validate (->VALIDATING; health+DB+queue+order+idempotency+manifest)
  -> Prepare Cutover (->READY_FOR_CUTOVER; gate: compat pass, drift 0 blocking, lag<=30, policy allow, approval, ownership aws)
  -> Shift Traffic (READY_FOR_CUTOVER->CUTTING_OVER; canonical stages 0/1/5/25/50/100 read-only until Stage 5)
  -> Verify (->VERIFYING; observed split + gates per stage)
  -> Complete / Rollback (->COMPLETED pre-write-rollback allowed; post-write traffic-only blocked)
```

Cutover sub-steps map to `docs/CUTOVER_ROUTING.md` 10-step per-stage procedure; queue sub-steps map to `docs/STATE_REPLICATION.md` pause/drain/verify.

## Migration engine responsibilities

- coordinate workflow steps (Temporal workflow; durable, retryable, idempotent per `run_id+step_id+attempt`)
- persist progress (control-plane PG; never application business data)
- honor policy gates (OPA `allow`/`requires_approval`/deny; fail-closed → PAUSED)
- record evidence (evidence IDs for every gate: validation report, drift report, compat report, lag sample, approval)
- invoke provider adapters (only path to cloud APIs; no direct calls)
- invoke validation (Validator in control-plane; CloudShop + contract checks)
- handle retries (bounded per step contract) and initiate pre-write rollback when policy/gates say so

Evidence schema per step: `{step_id, started_at, completed_at, evidence_ids[], policy_decision, approval_id, verification}` persisted on MigrationRun.

## It does not

- contain cloud-specific API logic (adapters do)
- make AI decisions directly (agents propose; engine executes deterministic plans)
- own application business data (CloudShop PG does)

Provider-specific operations belong in adapters. Rehearsal vs validation: rehearsal = full practice on ephemeral target without touching production traffic; validation = checks asserting readiness (can run inside or outside rehearsal).

## Execution implementation (control-plane `temporal.go`, migration `007_executions.sql`)

- Real Temporal SDK workflows (`MigrationStageWorkflow`) with mock-only activities; worker starts in-process when `TEMPORAL_HOST` is set, otherwise the execute endpoint fails closed.
- Deterministic workflow IDs per execution intent; per-migration concurrency guard with stale-row healing against live Temporal state.
- Preconditions revalidated inside the execution boundary (fresh deny wins); bounded retries (3 attempts, 30s start-to-close); deferred finalizer marks terminal state and audits on success and failure paths.
- Execution bookkeeping lives on the Migration model (`executions` rows); migration/workload lifecycle rows are never touched by execution. Mock activities mutate nothing by construction.
