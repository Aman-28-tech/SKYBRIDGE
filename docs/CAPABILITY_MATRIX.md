# Capability Matrix (local demo v1)

> Statuses are exactly PROVEN, PARTIALLY PROVEN, or DEFERRED.
> "Local" = demonstrated on one machine with project infrastructure.
> "Real" = requires AWS/Azure. Nothing below used real cloud.

| Capability | Status | Evidence | Local/Real | Notes |
|---|---|---|---|---|
| Workload registration | PROVEN | `demo-migration.sh` step 4; register/replay tests | Local | Canonical spec v1, server-side schema validation |
| Compatibility analysis | PROVEN | Step 7 (`conditional`); `TestCompat*` aggregation | Local | `pass\|conditional\|unknown\|block`, block-first aggregation |
| Migration planning | PROVEN | Step 8; `TestPlan*`; planner-v2 typed output | Local | Deterministic; fidelity-audited against compat |
| Drift detection | PROVEN | Step 9 (`clear`); scenario G live `blocking_drift` | Local | Desired/observed separated; `409 PLAN_STALE` on old plans |
| Policy gating | PROVEN | Step 10 (`approval_required`); `TestPolicy*` matrix | Local | Rego contract + Go mirror; time-based RPO gate |
| Approval workflow | PROVEN | Steps 11–12; scenario F; `TestApproval*` | Local | Human-distinct, evidence-bound, use-time revalidation |
| CDC replication | PROVEN | Steps 5–6, 14b (`CATCHUP_OK` measured); applier tests | Local | Debezium → Redpanda → canonical apply; dedupe counted |
| CDC lag measurement | PROVEN | `LAG_PROBE`, readiness `lag=2s`, cutover `cdc_lag_seconds` | Local | Measured per run, never asserted; local only |
| Reconciliation | PROVEN | Step 18; cutover `reconciliation.match=true` | Local | Probe-scoped by design (seed divergence excluded) |
| Rehearsal | PROVEN | Step 12 `REHEARSAL_READY`; `TestRehearsal*` | Local | Probes + replication + validation + policy, no mutation |
| Canary (read-only) | PROVEN | Step 13 stages 0–50 PASS; scenario E breach | Local | Stage-gated volume/windows; `expected_stage` guards workers |
| Write quiesce | PROVEN | Step 14 (503 on write); `quiesce_test.go` | Local | Ownership stays aws while writes pause |
| Ownership transfer | PROVEN | Step 17 `OWNERSHIP_TRANSFERRED`; scenario J exactly-once | Local | CAS commits once; source flips first, target second |
| Cutover | PROVEN | Step 17 `CUTOVER_COMPLETE`, 9 recorded stages | Local | Preflight revalidates everything; resumable blocks |
| Azure write / AWS reject | PROVEN | Step 18 `AZURE_WRITE_OK AWS_REJECTED_OK` | Local | Per-request ownership enforcement in both shops |
| Split-brain prevention | PROVEN | Step 18 agreement check; `TestSplitBrainInvariant` | Local | Ordered flips + enforcement; never dual-authoritative |
| Recovery / resume | PARTIALLY PROVEN | `cutover_recovery_test.go`; scenarios K–L | Local | Resume paths unit-proven; live mid-cutover restart not forced (timing hacks impractical) |
| Idempotency | PROVEN | SHA-256 `request_hash`, 409 conflicts, replays; scenario J | Local | Semantic replay; 24h expiry |
| Observability / evidence | PROVEN | Audit trail, `cutover.json`, status command | Local | Every transition audited with evidence hashes |
| Failure matrix A–L | PROVEN | `FAILURE_MATRIX = COMPLETE`, 12 scenarios, 10/10 result rows PASS (H+I and K+L share unit-proof rows) | Local | Live where deterministic, unit proofs otherwise |
| Deterministic reset | PROVEN | `RESET_OK`; repeated same-`RUN_ID` runs | Local | AWS authoritative restored; demo rows removed |
| Temporal execution | PARTIALLY PROVEN | SDK workflows, mock-only activities; fail-closed without `TEMPORAL_HOST` | Local | Skeleton only; full-lifecycle orchestration deferred |
| Terraform modules | PARTIALLY PROVEN | 14 modules `fmt`/`validate` green via `tf-plan.sh` | Local | `plan` needs creds (CI OIDC); `apply` never runs |
| Real AWS integration | DEFERRED | Fail-closed (`DescribeLiveEnvironment` refuses) | Real | Intentionally blocked; zero calls made |
| Real Azure | DEFERRED | Untouched | Real | Zero calls made |
| Front Door routing | DEFERRED | Local `routing` flag only | Real | Cutover switches local routing; no Front Door runs |
| Reverse CDC | DEFERRED | — | Real | Required before any post-authority rollback |
| Post-authority rollback | DEFERRED | Rejected by design (`POST_WRITE_ROLLBACK_BLOCKED`) | — | Resume-forward only in v1 |
| Source teardown | DEFERRED | Never automatic in v1 | — | By design |
| Console UI | PROVEN | Read-only Next.js console over live control-plane APIs; `SKYBRIDGE_CONSOLE = PASS` via `scripts/acceptance-console.sh` | Local | Observability layer only; GET-only, no mutation controls |
| AI planner/validator (advisory) | PROVEN | Mock-provider reviews over live control-plane evidence; `SKYBRIDGE_AI_ADVISORY = PASS` via `scripts/acceptance-ai.sh` | Local | Advisory only (`NEVER_BY_AI`); no authorization/mutation path; no production AI reliability claimed |
| Real-LLM review path | DEFERRED | Optional HTTP provider, unexercised | Real | Experimental; never required for tests/acceptance |
| Production RPO/RTO | DEFERRED | Local lag measured only | Real | Never claimed |
| Local/dev authentication (bearer) | PROVEN | `apps/control-plane/auth.go` + `TestAuth*`; demo/acceptance scripts mint per-run tokens | Local | Shared-secret lab auth only; OIDC/mTLS DEFERRED |
| Object-level authorization | PROVEN | `owner_id` + migration→workload binding + 403/404 tests | Local | Owner-or-admin model; no tenants/groups (DEFERRED) |
| Authenticated admin surface | PROVEN | CloudShop `auth.go` 401/403 tests; CP forwards service token | Local | Same local/dev caveat as above |
| Crash-safe ownership transfer | PROVEN | Commit-before-expose CAS + resume convergence + durable shop state tests | Local | Single-instance CP; multi-instance fencing DEFERRED |
| Production identity (OIDC/mTLS) | DEFERRED | — | Real | Required before any real-cloud exposure |
