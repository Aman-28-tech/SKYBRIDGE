# State Replication Strategy

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 2
> Last updated: 2026-09-25

## Strategy

Use:

```text
PostgreSQL
  |
  v
Debezium CDC
  |
  v
Kafka / durable event log
  |
  v
Replication Applier
  |
  v
Azure PostgreSQL
```

v1 is unidirectional (AWS → Azure). There is no tested reverse CDC in v1. This constrains rollback (see below).

## Initial load + CDC

The migration design is a full-load plus CDC pipeline.

```text
prepare schema (target reaches required schema_version first)
     |
start/verify CDC capture
     |
take consistent snapshot
     |
load target (parent/child order respected)
     |
consume CDC
     |
target catches up (cdc_lag_seconds <= RPO)
     |
validate (row counts, checksums, FKs, indexes, extensions)
     |
cutover (via write-quiesce sequence, not traffic alone)
```

The exact snapshot/stream ordering must be verified so there is no gap or duplicate window. Recommended: start CDC before snapshot, record snapshot LSN, replay events after snapshot LSN, then catch up.

## WAL / logical decoding

PostgreSQL records changes in the Write-Ahead Log (WAL).

CDC uses logical decoding to expose committed changes in a form a consumer can process. Clocks on source, applier, and observer must be synchronized (NTP) sufficiently for the lag metric to be meaningful (±1s or better for a 30s gate).

## LSN

An LSN is a PostgreSQL log sequence position. Track `source_lsn` and `target_applied_lsn` separately from the time-based RPO metric. LSN answers "how far", time answers "how stale".

## CDC freshness (RPO metric — canonical definition)

CloudShop requirements (design targets, not claims):

```text
RPO = 30 seconds
RTO = 15 minutes
```

Canonical lag metric:

```text
cdc_lag_seconds =
    target_apply_observation_time
    -
    source_transaction_commit_timestamp
```

- `source_transaction_commit_timestamp`: commit time of the latest transaction applied on target.
- `target_apply_observation_time`: wall-clock time the measurement is taken (observer clock, NTP-synced).
- Track also: `source_lsn`, `target_applied_lsn`, `event offset`, `last applied transaction id`, `applied_events_total`.

Cutover RPO gate (final sync, before write-ownership transfer):

```text
observed cdc_lag_seconds <= 30
```

RTO measurement: starts at the documented failure/cutover trigger, ends when target serves valid traffic AND `/healthz` + `/readyz` pass AND critical validation passes. Measure in rehearsal; do not claim until measured.

## CDC event identity

Each change event must carry enough identity to support:

- ordering where required
- duplicate detection
- retry
- audit
- replay

The consumer stores its durable offset/applied position. Event envelope must include `request_id`, `run_id`, `workload_id`, `schema_version` where applicable (see `migration.proto`).

## Applier idempotency

The applier must not double-apply a change after:

```text
consumer timeout
consumer restart
event redelivery
batch partial failure
```

Use a durable applied-event/transaction marker or an equivalent idempotency mechanism (e.g. applied-txn table with PK on source txn id).

## DDL / schema changes

Data CDC does not replace schema migration. See `docs/SCHEMA_MIGRATION.md`.

Schema changes are version-controlled and forward-compatible for the migration window. Target must reach the required `schema_version` before CDC cutover. The application publishes `schema_version` via `/readyz` payload and a `schema_version` table; the applier refuses cutover if versions skew.

Do not introduce incompatible DDL in the middle of a migration without a separately tested strategy.

## Sequences

PostgreSQL sequences may not be naturally represented by row-level CDC alone.

Therefore CloudShop uses UUID identifiers for application entities in v1. This reduces sequence divergence risk during cross-cloud replication.

## Foreign keys

Because the target load must respect relational constraints:

- initial load ordering must account for parent/child relationships (`users` → `orders` → `order_items`)
- or constraints must be deferred only where explicitly safe and documented per-table
- validation must include referential integrity (`FOREIGN KEY ... VALID`), index presence, and extension checks

## Transactions

A transaction containing multiple row changes should preserve enough transaction context to apply related changes safely (atomic apply per source transaction where feasible).

## Object storage (v1 semantics)

Identity is logical application key + content hash, NOT provider version ID:

```text
source manifest (key -> sha256, size, updated_at)
   |
copy objects
   |
target manifest
   |
reconcile (missing, hash-mismatch, orphan, delete-marker)
```

- Object identity: logical key (`products/{id}/images/*`, `exports/{id}/result.json`).
- Content validated by SHA-256. Provider-native version IDs may be enabled as a safety mechanism but are NOT part of the cross-cloud identity model and need not match.
- Deletes are represented in the logical manifest/reconciliation (tombstone or expected-absent list), not by replicating provider delete markers.
- Must account for: creates, replacements, deletes, metadata (content-type), incomplete/multipart transfers.

## Queue (v1 sequence)

CloudShop queue: `at-least-once`, NO strict global ordering requirement, workers duplicate-safe via durable `job_id` (`order_id` for order jobs, `export_id` for exports).

Final cutover sequence:

```text
1. pause producers (API stops enqueueing new jobs; returns 503 QUEUE_PAUSED where applicable)
2. drain: workers continue until queue depth <= 10 (strict: 0) for 60s sustained
3. verify duplicate-safe replay (re-deliver sample, assert no double side effect)
4. cut over (traffic + write ownership per CUTOVER_ROUTING.md)
5. resume producers on target
6. verify depth stays bounded and no orphaned source jobs remain
```

## Redis (v1: lazy warm)

Redis is cache-only, never authoritative. Key: `product:{id} -> cached product JSON`, TTL `300s`.

Target starts cold. First request misses, reads PostgreSQL, populates Redis. No manual cache replication. `readyz` does NOT require warm cache; it requires DB + queue publish/consume healthy.

## Rollback / failback (v1 rule)

Pre-write-ownership rollback = allowed (traffic back to AWS; AWS still authoritative; Azure shadow discarded or reconciled later).
Post-write-ownership traffic-only rollback = BLOCKED unless reverse sync is implemented and tested. After Azure takes writes, AWS is stale; routing traffic back without reverse replication loses data. Use forward-fix/recovery. See `docs/CUTOVER_ROUTING.md` and `docs/WORKFLOW_STATE_MACHINE.md`.

## Recovery

The replication pipeline must survive:

- Debezium restart
- broker restart
- applier restart
- target database temporary failure
- lag spikes

and resume from durable positions (offset + applied-txn marker + LSN). Each recovery path needs a chaos test (see `docs/TESTING_CHAOS.md`).
