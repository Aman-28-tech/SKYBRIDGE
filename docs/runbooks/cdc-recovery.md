# Runbook — CDC Recovery

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-25

## Purpose

Restore CDC (Debezium → log → applier → target PG) from durable positions without double-apply.

## Preconditions

- [ ] `run_id`, connector/consumer/applier logs, `source_lsn`, `target_applied_lsn`, `cdc_lag_seconds` captured
- [ ] Target PG reachable; schema versions match (else see schema runbook path)

## Commands (placeholders)

```bash
# inspect (examples; confirm deployment topology first)
kubectl -n skybridge logs deploy/debezium --since=30m
kubectl -n skybridge logs deploy/cdc-applier --since=30m
# offsets + applied-txn marker (psql examples; confirm host/secret source first)
psql "$TARGET_PG" -c 'SELECT * FROM cdc_offsets ORDER BY updated_at DESC LIMIT 5;'
psql "$TARGET_PG" -c 'SELECT count(*) FROM cdc_applied_txn;'
# restart is via rollout, never blind re-apply:
kubectl -n skybridge rollout restart deploy/cdc-applier
```

Do not invent cloud commands; verify topology before running.

## Verification

- [ ] Consumer resumes from durable offset; applier skips already-applied txns (marker hits, no double business effect)
- [ ] `cdc_lag_seconds` trends down and sustains <= 30 before cutover gate re-arms
- [ ] Row-count/checsum spot check passes

## Failure symptoms

- Lag stuck high → broker retention/partition skew, Debezium task failure, clock skew, long transaction blocking logical decoding
- Duplicate business rows → applied-marker gap; stop, fix marker write path, replay from last committed marker

## Rollback/recovery

- If target corrupted: rebuild target from fresh snapshot + CDC replay (record new `run_id`); never patch rows by hand without audit

## Cleanup

- [ ] Incident timeline + lag chart retained; retention/partition fix filed as follow-up

## Owner

Data-plane operator.
