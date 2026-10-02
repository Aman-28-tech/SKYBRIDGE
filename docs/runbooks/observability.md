# Runbook — Observability Triage

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-25

Supersedes the stub checklist with an executable path. (Canonical triage path; `docs/OBSERVABILITY_RUNBOOK.md` is a pointer here.)

## Purpose

Diagnose slow/failed migrations and cutovers via `run_id` correlation without single-metric guessing.

## Preconditions

- [ ] `run_id` (+ `request_id`/`trace_id` where available)

## Path

```text
run_id -> workflow trace (Temporal) -> step (duration/retries) -> provider call (throttle/errors) -> validation report -> policy/audit decision
```

If migration is slow, check in order: step duration, cloud API latency + throttle counters, `cdc_lag_seconds` + LSN gap, queue depth, target capacity (CPU/mem/connections), validation duration.

If cutover fails, check: routing change result (desired vs observed), error rate vs threshold, p95/p99 vs thresholds, app health (`/healthz`/`/readyz`), DB health + lag, queue, policy/audit (`allow` vs `deny` + `deny_reason`).

Do not infer a root cause from one metric. Minimum evidence: trace span + failing gate sample + audit decision for the same `run_id`.

## Verification

- [ ] Fixing change tied to a gate (lag down, error down, drift resolved) sustained over the stage window before advancing

## Cleanup

- [ ] Triage notes + dashboard links attached to the migration run record

## Owner

On-call migration operator.
