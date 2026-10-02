# SKYBRIDGE Demo Guide (5–10 minute presentation)

> Local only. Zero cloud calls, zero spend. The reviewer watches real
> state transitions driven by `./scripts/showcase.sh` (or the steps
> below), optionally with the read-only console open on `/demo`.
>
> One-command presentation wrapper (successful migration only, evidence
> left running for the console): `./scripts/portfolio-demo.sh`
> (doctor → reset → lab up → `demo-migration.sh` → status).
> Full verification wrapper (migration + failure matrix + reset):
> `./scripts/showcase.sh`.

## Minute 0–1 — Health check

```bash
./scripts/doctor.sh
docker compose up -d && docker compose --profile cdc up -d
```

Say: "One machine runs both clouds as fixtures: source Postgres on
:5433, target on :5434, Debezium, Redpanda, and the control plane.
Real AWS/Azure are never touched — adapters report LOCAL_FIXTURE and
`ApplyEnabled=false` is a compile-time constant."

Watch for: `DOCTOR = PASS`.

## Minute 1–2 — Reset to a known state

```bash
./scripts/reset-demo.sh
```

Say: "Reset is deterministic: AWS authoritative, Azure standby, demo
rows removed." Watch for: `RESET_OK`.

## Minute 2–6 — The migration

```bash
./scripts/showcase.sh
```

This runs doctor → reset → `demo-migration.sh` → `demo-failures.sh` →
reset → status. Narrate the demo landmarks as they print:

- **Step 4, register**: canonical workload spec v1, server-side schema
  validation, idempotent replay.
- **Steps 5–6, traffic + CDC**: deterministic users/orders on the
  source; Debezium/Redpanda topics carry them (`CDC_TOPICS_OK`).
- **Step 7–9, compatibility → plan → drift**: `conditional` status
  (block-first aggregation), deterministic `planner-v2` plan, `clear`
  drift gate.
- **Steps 10–12, policy → approval → rehearsal**: `approval_required`,
  measured `LAG_PROBE`, human-distinct approval, `REHEARSAL_READY`.
- **Step 13, canary 0–50**: read-only stages with volume/window gates.
- **Step 14, quiesce + catch-up**: source writes return 503;
  `CATCHUP_OK` drains traffic through the idempotent apply path with
  measured `captured/applied/duplicates`.
- **Steps 15–17, readiness → cutover**: `READY_FOR_CUTOVER`, then
  `CUTOVER_COMPLETE` with 9 recorded stages and Azure authoritative.
- **Step 18, verify**: `AZURE_WRITE_OK`, `AWS_REJECTED_OK` (403),
  reconciliation `MATCH`, no split brain.

With the console open (`cd apps/console && npm run dev`, page
`/demo`), the lifecycle and cutover flows light up from the live APIs
— no animation pretends progress; stages render only when the API
reports them.

## Minute 6–8 — Failure scenarios

`./scripts/demo-failures.sh` runs 12 scenarios A–L, ending
`FAILURE_MATRIX = COMPLETE` (10/10 result rows; H+I and K+L share
unit-proof rows where live forcing needs timing hacks):

| Scenario | Injects | Proves |
|---|---|---|
| A | target DB stopped | readiness NOT_READY, no transfer, recovery |
| B | CDC consumer stopped | rehearsal BLOCKED, resumes after restart |
| C | CDC lag over RPO | deny with `rpo_breach` (unit proof) |
| D | reconciliation mismatch | `validation_failed`, no transfer (unit proof) |
| E | canary 5xx breach | stage FAIL, advancement refused |
| F | missing approval (conditional) | `APPROVAL_REQUIRED`, no transfer |
| G | blocking drift | policy deny before data-plane work |
| H/I | stale compat/plan | `COMPATIBILITY_STALE` / `PLAN_STALE` (unit proofs) |
| J | concurrent cutovers, same key | exactly one transfer, identical bytes |
| K | worker restart mid-cutover | resume to exactly one transfer (unit proof) |
| L | partial ownership flip | paused-safe, resume completes (unit proof) |

Say: "Every refusal is fail-closed — the system denies or pauses, and
recovery always moves forward from authoritative state."

## Minute 8–10 — Evidence and close

```bash
./scripts/skybridge-status.sh
```

Walk the architecture / migration / CDC / safety / cloud summary.
Close with: "Proven locally, documented honestly — `PROVEN`,
`PARTIALLY PROVEN`, `DEFERRED` in `docs/CAPABILITY_MATRIX.md`. Local
proof is not cloud proof."
