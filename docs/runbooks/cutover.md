# Runbook — Cutover (canonical stages)

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-25

## Purpose

Advance the canonical read-only canary (0→1→5→25→50→100 with write transfer before 100).

## Preconditions

- [ ] `READY_FOR_CUTOVER` gate all-green (compat pass, drift 0 blocking, health pass, validation pass, lag<=30, policy allow, approval, ownership aws)
- [ ] Baseline window captured (15 min, same load profile, volume recorded)
- [ ] Front Door Standard origin group healthy (both origins probed)

## Commands (placeholders)

```bash
# stage N (weights restricted to 0/1/5/25/50/100; Idempotency-Key required)
curl -X POST /api/v1/migrations/<id>/cutover -H 'Idempotency-Key: <uuid>' -d '{"target_weight":1}'
# observe (per-stage window 5/15/30/60 min + min volume 300/100)
# repeat for 5, 25, 50; Stage 100 only after write-transfer sequence verified
```

## Verification (per stage)

- [ ] Desired vs observed split recorded (Front Door logs + app metrics)
- [ ] `target_5xx <= max(1%, baseline+0.5pp)`, `p95 <= max(500ms, baseline*1.25)`, `p99 <= max(1s, baseline*1.25)`
- [ ] DB/CDC (`cdc_lag_seconds`), queue depth, drift, policy/approval all green
- [ ] Inconclusive (low volume) → hold, do not advance

## Failure symptoms

- Gate breach → workflow PAUSED; do not auto-advance. Diagnose via `run_id → trace → step → provider → validation` path.
- Writes seen on Azure before ownership → immediately pause; audit `WRITE_NOT_OWNED` denials; treat as compat/test failure.

## Rollback/recovery

- Pre-write: `POST /v1/migrations/<id>/rollback` (Azure→0, AWS full, verify AWS authoritative). Post-write without reverse sync: expect `409 POST_WRITE_ROLLBACK_BLOCKED`; use forward-fix (see rollback runbook).

## Cleanup

- [ ] Stage history + observed ratios retained in audit; Front Door weights left at final verified state only

## Owner

Migration operator; approver required for staging >25% and all production-like stages.
