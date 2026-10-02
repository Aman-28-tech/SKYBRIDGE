# ADR — Local Demo Safety Model (implemented semantics)

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-28
>
> Consolidates the safety-relevant decisions already implemented in code
> and proven by the local demo + failure matrix. Each entry states the
> actual implementation semantics with file references, not aspirations.

## 1. Durable vs ephemeral state

Decision: durable state lives in PostgreSQL and Redpanda; process memory
is ephemeral by design. The control-plane `Store` interface
(`apps/control-plane/main.go:45-46`) has `MemStore` (default) and
`PGStore` (when `DATABASE_URL` is set). The local demo runs the
control-plane in-memory — a fresh control-plane is therefore a fresh
world, while Redpanda topics, consumer groups, and CloudShop Postgres
persist across invocations. Demo scripts account for exactly this split:
rehearsal idempotency keys carry a per-invocation nonce (broker groups
persist), CloudShop checks parse JSON semantically (jsonb replays),
data IDs stay deterministic per `RUN_ID`. Evidence:
`scripts/demo-migration.sh` (`INV`, `HEX_SUFFIX`), full demo +
`FAILURE_MATRIX = COMPLETE` on repeated same-`RUN_ID` runs.

## 2. CDC event identity

Decision: `EventID` is deterministic over source ordering identity
(table, primary key, LSN/transaction position) and never over wall-clock
or arrival time (`apps/cdc-applier/event.go:123-128`; header
`event.go:4`). The same source change always maps to the same event ID
no matter when or how often it is consumed. Evidence:
`TestEventIDDeterminism`, `TestEventIDIgnoresWallClock`.

## 3. CDC deduplication

Decision: `cdc_applied_events(event_id)` is the authoritative per-event
dedupe record; a redelivered event is counted as a duplicate and never
double-applied (`apps/cdc-applier/apply.go`, `engine.go:62-68`). The
demo catch-up routinely reports `duplicates ≫ applied` (e.g.
`captured=99 applied=9 duplicates=90`) — that ratio is the dedupe proof,
not waste. Evidence: `TestDuplicateAfterRestartNoDoubleApply`,
`TestAppliedSetDedupe`, live `CATCHUP_OK` lines.

## 4. Offset progression

Decision: checkpoints advance only after a committed apply and never
rewind past applied work (`apps/cdc-applier/checkpoint.go:115`);
an event older than the checkpoint that was never applied fails
explicitly (`ErrOutOfOrder`, `engine.go`) for retry/inspection rather
than silent reordering. Arrival time never influences ordering or
identity. Evidence: `TestCheckpointNeverRewinds`, `TestRestartResume`,
`TestInOrderAcrossTransactions`, `TestOutOfOrderRejected`.

## 5. Compatibility semantics

Decision: status is exactly one of `pass | conditional | unknown |
block`, aggregated `block > unknown > conditional > pass`; only
`overall == pass` may enter automatic cutover readiness, `conditional`
always requires explicit approval (`apps/control-plane/compat.go`,
`docs/CLOUD_COMPATIBILITY_ENGINE.md`). The demo workload evaluates
`conditional` by design, so every demo cutover passes through approval.
Evidence: `TestCompat*` aggregation tests, demo step 7 + 10
(`approval_required`).

## 6. Drift semantics

Decision: desired state and observed state are separate records; observed
state never overwrites desired state. Severities are `informational |
blocking | security_critical`; blocking (or security_critical) drift
bars `READY_FOR_CUTOVER`. Plans carry machine-typed versions: drift
rejects pre-v2 plans with explicit `409 PLAN_STALE` instead of false
findings. Evidence: `drift.go`, `drift_test.go`, failure-matrix
scenario G (live `blocking_drift`).

## 7. Policy ordering

Decision: Rego (`packages/contracts/policy/cutover.rego`) is the
contract; the Go evaluator (`apps/control-plane/policy.go`) is a
rule-by-rule mirror with deny reasons matching `deny_reason` values
one-for-one. Decisions are `allow | deny | approval_required`, evaluated
over compat + drift + health + freshness + validation + stage/ownership.
No valid decision ⇒ deny, workflow paused — never fail open. Freshness
is time-based (`cdc_lag_seconds <= rpo_seconds`), not LSN-based.
Evidence: `TestPolicy*` matrix, demo steps 10–11 (`LAG_PROBE`,
`POLICY_DENIED` on genuine 1068s stale capture during testing).

## 8. Approval model

Decision: approvals are explicit, human-distinct-from-requester, and
bound to evidence via `PolicyInputHash` (`apps/control-plane/approval.go:184`).
Every use revalidates approval state, expiry, hash equality
(`APPROVAL_STALE`), evidence staleness, and deny-at-use
(`approval.go:363-390`). Measured evidence (including CDC lag) is part
of the hash, so approvals go stale when the world moves; callers
re-approve at fresh measurements instead of bypassing. Approval never
executes. Evidence: `TestApproval*`, demo steps 11–12/15–16 re-approve
loops, scenario F (`APPROVAL_REQUIRED`), scenario J (`APPROVAL_STALE`
recovery).

## 9. Idempotency semantics

Decision: every mutating call carries an `Idempotency-Key`; the server
stores SHA-256 `request_hash` with the response (`store.go:24`). Same
key + same body ⇒ byte-semantic replay; same key + different body ⇒
`409 IDEMPOTENCY_CONFLICT` (`main.go:75-87`); keys expire after 24h.
Replay is semantic (status + deep-equal JSON), not byte identity, because
Postgres jsonb normalizes formatting. Evidence: idempotency A–F tests,
same-`RUN_ID` demo reruns, scenario J (concurrent identical cutovers ⇒
byte-identical responses, single transfer).

## 10. Ownership CAS

Decision: the authoritative transfer is one store compare-and-swap,
`TransferOwnership aws -> azure`, which commits exactly once; retries
and concurrent callers observe the single committed result
(`apps/control-plane/cutover_final.go:19,271`). Shops additionally
enforce the single-writer invariant per request (`WRITE_NOT_OWNED` 403).
Evidence: scenario J (one transfer, byte-identical responses),
`TestSplitBrainInvariant`, ownership tests.

## 11. Source-first ownership flip

Decision: the source admin flips `aws -> azure` first, the target flips
second (`cutover_final.go:15-18,220,256`). The window between flips is
both-reject (safe); there is never a dual-authoritative window. A
failure between flips leaves a paused state retried via the resume path.
Evidence: demo step 17 stage list (`OWNERSHIP_TRANSFERRED` before
`TRAFFIC_SWITCHED`/`WRITES_RESUMED`), K–L resume proofs.

## 12. Split-brain prevention

Decision: by construction (ordered flips + per-request ownership
enforcement + post-flip agreement verification), not by monitoring.
After cutover both shops must report the same owner; any disagreement
is a hard failure, and Azure writes + AWS rejections are verified live
(demo step 18: `AZURE_WRITE_OK AWS_REJECTED_OK`, ownership agreement,
`SPLIT BRAIN PREVENTED`). Evidence: `TestSplitBrainInvariant`,
`TestOwnershipEnforcementProof`, acceptance `SPLIT_BRAIN` magnet.

## 13. Rollback restriction

Decision: v1 has no reverse CDC, so after Azure becomes the authoritative
writer, traffic-only rollback to AWS is rejected
(`cutover_final.go:11-12,170`; `409 POST_WRITE_ROLLBACK_BLOCKED`
wiring in `main.go`). Recovery is resume-forward. Source infrastructure
is never destroyed automatically. Evidence: cutover audit tests,
`docs/CUTOVER_ROUTING.md`.

## 14. Terraform Apply boundary

Decision: three independent locks, all default-deny. (1)
`ApplyEnabled=false` (`apps/control-plane/tfvars.go:19`) with the
adapter `ApplyInfrastructure` path refusing unconditionally.
(2) No script runs `terraform apply/destroy` — `tf-plan.sh` does
`fmt + validate` only, and `plan` itself needs provider credentials
that exist only in CI via OIDC. (3) The acceptance `NO_CLOUD_CALLS`
magnet greps for SDK imports and apply/destroy invocations on every
run. Evidence: `TestAdapter*` apply-refusal tests, every acceptance run.

## Trade-offs

In-memory demo control-plane trades cross-invocation continuity for
hermetic reruns (accepted: Redpanda + Postgres carry the durable
truth). Probe-scoped reconciliation trades full-table gating for
robustness against pre-existing seed divergence (accepted: probes are
the migration's own evidence). Time-based RPO trades LSN precision for
a directly measurable freshness signal (accepted: LSNs are still
recorded as evidence).
