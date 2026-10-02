# SKYBRIDGE Engineering Story

> What made this project technically interesting, and what was actually
> built. Every mechanism below names its implementation. No exaggerated
> claims: local proof is not cloud proof.

## 1. Stateful migration vs stateless deployment

Deploying stateless code to another cloud is a solved problem (build,
push, route). Migrating a **stateful** workload — CloudShop: app +
PostgreSQL + Redis + object storage + queue — means the data plane has
a memory: orders, users, offsets, and in-flight writes must survive the
move with zero loss and zero double-apply. SKYBRIDGE therefore treats
migration as a **continuity** problem governed by a control plane, not
as a provisioning script. The unit of migration is the canonical
workload spec (`packages/contracts/schemas/canonical-workload.schema.json`,
schema_version 1), and every step from registration to cutover is a
gated, audited transition.

## 2. PostgreSQL CDC (Debezium → Redpanda → applier)

The source database runs with `wal_level=logical`. A Debezium
connector (`debezium/`, Kafka Connect on the `cdc` compose profile)
streams row changes into Redpanda topics (`cloudshop.public.users`,
`cloudshop.public.orders`, …). `apps/cdc-applier` consumes through the
canonical path (`FromDebezium` + `PostgresStore.ApplyAtomically`):
parents-before-children topic order keeps foreign keys safe,
tombstones are skipped, and `cmd/cdc-catchup` drains pending traffic
after quiesce. Nothing here touches a cloud API.

## 3. Durable event deduplication

Redelivery is normal (restarts, replays, rebalances), so identity must
come from the source, never from arrival time. `EventID` is
deterministic over source ordering identity (table, primary key,
LSN/transaction position — `event.go`). `cdc_applied_events(event_id)`
is the authoritative dedupe table: replays are **counted** as
duplicates and never double-applied
(`TestDuplicateAfterRestartNoDoubleApply`).

## 4. Offsets that never rewind

Checkpoints advance only after a committed apply and never move past
applied work (`checkpoint.go`); out-of-order arrivals fail explicitly
for retry/inspection (`engine.go`) instead of silently reordering.

## 5. Measured RPO

Lag is **measured** per run (source commit timestamp → apply time,
`lag.go`, RPO boundary 30s from the workload spec) — never asserted
into existence. The policy freshness gate, readiness `cdc_catchup`,
rehearsal `RPODecision`, and the CDC report `WithinRPO` all resolve
the threshold from the same workload-spec value (release-audit fix +
`TestReadinessCustomRPO`), so the gates cannot contradict each other.

## 6. Probe-scoped reconciliation

`rehearsal_cdc.go` compares only the **probe set** between source and
target; table counts are context. Full-table comparison is deliberately
not gated because the two lab databases carry pre-existing seed
divergence (e.g. independently seeded `products` SKUs) unrelated to
migration traffic. Cutover requires `reconciliation.match=true`.

## 7. Deterministic plans

The planner (`planner-v2`) deterministically derives the target plan
from the compatibility report; a fidelity audit preserves material
spec values (CPU/memory/AZ/RPO/RTO/flags) and never invents optional
ones. Machine-typed output is enforced: drift rejects pre-v2 plans
with explicit `409 PLAN_STALE` instead of reporting false findings.

## 8. Stale evidence (fail-closed freshness)

Desired state and observed state are separate records; observations
never overwrite intent. Approvals bind `PolicyInputHash` (which
includes measured values like CDC lag), so any world movement makes
them stale (`APPROVAL_STALE`) and the demo re-approves rather than
weakening the check. Re-observing identical state refreshes
`detected_at` without duplicating rows.

## 9. Policy and approval

`packages/contracts/policy/cutover.rego` is the contract; the Go
evaluator is a rule-by-rule mirror with one-for-one deny reasons.
Decisions are exactly `allow | deny | approval_required`, and an
undecidable engine denies and pauses — never fail open. Approvals are
explicit, human-distinct-from-requester, evidence-bound, and
revalidated at use time. Approval never executes anything.

## 10. Idempotency

All mutations require `Idempotency-Key`; the server stores a SHA-256
`request_hash` and returns `409` on same-key/different-body, with
semantic (deep-equal JSON, not byte-identical) replay and 24h expiry.

## 11. Ownership CAS and split-brain prevention

Exactly one writer exists by construction. Transfer is a store CAS
(`TransferOwnership aws → azure`) committing once — concurrent
cutovers return byte-identical responses (scenario J). The source
flips first, the target second; a crash between flips leaves a paused,
resumable state, never dual-authoritative. Both shops enforce
`WRITE_NOT_OWNED` (403) per request (`TestSplitBrainInvariant`).

## 12. Failure recovery (resume-forward)

Worker-restart resume, partial-flip pause + resume, and
quiesce/CDC/verify failure paths are proven (`cutover_recovery_test.go`,
scenarios K–L). There is no reverse CDC in v1, so post-authority
traffic-only rollback is rejected (`POST_WRITE_ROLLBACK_BLOCKED`) —
recovery moves forward from authoritative Azure state.

## 13. Canary and write quiesce

Canonical stages `0/1/5/25/50/100`; stages 0–50 are read-only
evaluations with per-stage volume (≥300 requests, ≥100 for stage 1)
and observation windows, guarded by `expected_stage` against stale
workers. Quiesce pauses writes (source POSTs return `503
WRITES_PAUSED`) while ownership stays `aws`, so final catch-up drains
a frozen source.

## 14. AI authority separation

The reviewer (`apps/agent-service/skybridge_ai`) reads evidence
through GET endpoints only, works under prompt `ai-planner-v1`, and
returns a schema-fixed review with `authorization: NEVER_BY_AI`. The
validator rejects fabricated evidence references, missing references,
bad enums, malformed JSON, and any authoritative `decision` value.
Workload/application data is framed as UNTRUSTED CONTENT; secrets are
stripped before the model call. No review output reaches policy,
approval, ownership, cutover, Terraform, or cloud APIs — verified by a
live audit-trail-unchanged check.
