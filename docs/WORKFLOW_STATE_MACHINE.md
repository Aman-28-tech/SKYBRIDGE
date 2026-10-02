# Migration Workflow State Machine

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 2
> Last updated: 2026-09-25

## States

```text
REGISTERED
   |
DISCOVERED
   |
MODELED
   |
COMPATIBLE
   |
PLANNED
   |
APPROVED
   |
PROVISIONING
   |
TARGET_READY
   |
REPLICATING
   |
REHEARSING
   |
VALIDATING
   |
READY_FOR_CUTOVER
   |
CUTTING_OVER
   |
VERIFYING
   |
COMPLETED
```

Failure/recovery:

```text
Any non-terminal state
   |
   +--> RETRYING   (transient fault, bounded retries remain)
   +--> PAUSED     (policy unavailable, approval pending, or operator hold)
   +--> FAILED
           |
           +--> RECOVERY
           +--> ROLLING_BACK
                    |
                    v
                ROLLED_BACK
           |
           +--> ABORTED
```

Entry rules:

- `RETRYING`: retryable error (throttle, temp DB failure) AND attempt < max_attempts. Backoff: 5s × 2^attempt, max 5 attempts per step unless step contract says otherwise.
- `PAUSED`: policy engine unavailable, `APPROVAL_REQ` outstanding, or operator pause. Never auto-resume a PAUSED mutation without re-evaluating policy.
- `FAILED`: non-retryable error or retries exhausted.
- `ROLLING_BACK`: only from `CUTTING_OVER`/`VERIFYING` pre-write, or from any state where no Azure writes occurred. Post-write `ROLLING_BACK` (traffic-only) is denied by policy unless reverse sync is implemented and tested.
- `ABORTED`: operator-aborted or policy-denied terminal stop. Retains history; does not delete source.

## State invariants

Every state transition must be:

- durable
- explicit
- auditable
- validated
- idempotent where possible

## Step contract

Each step defines:

```text
timeout
retry policy (max attempts, backoff)
idempotency behavior (migration_run_id + step_id + attempt_scope)
failure code
rollback behavior (pre-write vs post-write)
verification
cleanup behavior
```

## Example timeouts

These are initial design values for development experiments, not measured production guarantees:

```text
discovery: 5 min per discovery unit (one account + one region scan)
provisioning activity: 20 min per bounded scope (network OR eks OR db — not per resource)
validation suite: 10 min
cutover stage observation: per canonical canary table (5/15/30/60 min), not a flat 5 min
```

Actual timeouts must be tuned using measured runs.

## Terminal states

```text
COMPLETED
ROLLED_BACK
ABORTED
```

Terminal state cleanup must:

- stop temporary rehearsal infrastructure
- close temporary connections
- mark transient resources for deletion
- verify cleanup
- retain migration/audit history

## Cutover gate

`VALIDATING -> READY_FOR_CUTOVER` requires ALL of:

```text
compatibility overall = pass            (conditional requires explicit human approval; unknown/block deny)
open blocking drift = 0                 (blocking + security_critical both count; informational never blocks)
open security_critical drift = 0
target health = pass
required validation = pass              (healthz + readyz + DB + queue + order flow + idempotency replay)
cdc_lag_seconds <= 30                   (RPO gate; see STATE_REPLICATION.md for metric definition)
policy = allow                          (OPA cutover.rego allow == true)
required approval = approved            (production-like always; staging stage>25%; see POLICY_ENGINE.md)
write ownership = aws                   (Azure must still be read-only/shadow; see CUTOVER_ROUTING.md)
```

`replication lag` here means `cdc_lag_seconds = target_apply_observation_time - source_transaction_commit_timestamp` (time-based freshness). LSN/offset positions are tracked separately and must also be advancing.

## Canary stages (canonical — authority is CUTOVER_ROUTING.md)

```text
Stage 0:   0% target, baseline
Stage 1:   1% target read-only,  observe 5 min
Stage 2:   5% target read-only,  observe 15 min
Stage 3:  25% target read-only,  observe 30 min
Stage 4:  50% target read-only,  observe 60 min
Stage 5: 100% target, final validation after write-ownership transfer
```

Stages 1–4 are read-only canary: CloudShop write requests MUST NOT use Azure as authoritative writer. Stage 5 follows the write-quiesce sequence (stop writes → drain → CDC catch-up → RPO gate → transfer ownership → switch → verify → resume).

## Rollback rule (v1)

```text
pre-write-ownership rollback  = ALLOWED  (Azure weight -> 0, AWS stays authoritative)
post-write-ownership rollback = BLOCKED unless reverse synchronization is implemented and tested
```

After Azure becomes the authoritative writer, a simple traffic rollback is NOT automatically allowed. Use forward-fix/recovery. `rollback_traffic()` after write transfer must return `409 POST_WRITE_ROLLBACK_BLOCKED` unless `reverse_sync_ready == true`.

## Restart behavior

If a worker dies:

```text
Temporal resumes workflow
   |
inspect current durable state
   |
determine whether step already completed
   |
continue safely
```

Do not blindly repeat a side-effecting step.

## Idempotency key

Every mutation step receives:

```text
migration_run_id + step_id + attempt_scope
```

persisted as `Idempotency-Key` with `SHA-256(request body)` fingerprint, 24h retention, and its provider operation identifier where available. Same key + different body → `409 IDEMPOTENCY_CONFLICT`.
