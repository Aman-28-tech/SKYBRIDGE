// Capability matrix snapshot for the Evidence view. Mirrors
// docs/CAPABILITY_MATRIX.md (local demo v1). Statuses are exactly
// PROVEN | PARTIALLY PROVEN | DEFERRED. The console renders this static
// data; it never claims live proof beyond what the control-plane APIs and
// the local demo artifacts report.
export type CapabilityStatus = "PROVEN" | "PARTIALLY PROVEN" | "DEFERRED";

export type CapabilityRow = {
  capability: string;
  status: CapabilityStatus;
  evidence: string;
  scope: string;
  notes: string;
};

export const CAPABILITY_MATRIX: CapabilityRow[] = [
  { capability: "Workload registration", status: "PROVEN", evidence: "demo-migration.sh step 4; register/replay tests", scope: "Local", notes: "Canonical spec v1, server-side schema validation" },
  { capability: "Compatibility analysis", status: "PROVEN", evidence: "Step 7 (conditional); TestCompat* aggregation", scope: "Local", notes: "pass|conditional|unknown|block, block-first aggregation" },
  { capability: "Migration planning", status: "PROVEN", evidence: "Step 8; TestPlan*; planner-v2 typed output", scope: "Local", notes: "Deterministic; fidelity-audited against compat" },
  { capability: "Drift detection", status: "PROVEN", evidence: "Step 9 (clear); scenario G live blocking_drift", scope: "Local", notes: "Desired/observed separated; 409 PLAN_STALE on old plans" },
  { capability: "Policy gating", status: "PROVEN", evidence: "Step 10 (approval_required); TestPolicy* matrix", scope: "Local", notes: "Rego contract + Go mirror; time-based RPO gate" },
  { capability: "Approval workflow", status: "PROVEN", evidence: "Steps 11-12; scenario F; TestApproval*", scope: "Local", notes: "Human-distinct, evidence-bound, use-time revalidation" },
  { capability: "CDC replication", status: "PROVEN", evidence: "Steps 5-6, 14b (CATCHUP_OK measured); applier tests", scope: "Local", notes: "Debezium -> Redpanda -> canonical apply; dedupe counted" },
  { capability: "CDC lag measurement", status: "PROVEN", evidence: "LAG_PROBE, readiness lag, cutover cdc_lag_seconds", scope: "Local", notes: "Measured per run, never asserted; local only" },
  { capability: "Reconciliation", status: "PROVEN", evidence: "Step 18; cutover reconciliation.match=true", scope: "Local", notes: "Probe-scoped by design (seed divergence excluded)" },
  { capability: "Rehearsal", status: "PROVEN", evidence: "Step 12 REHEARSAL_READY; TestRehearsal*", scope: "Local", notes: "Probes + replication + validation + policy, no mutation" },
  { capability: "Canary (read-only)", status: "PROVEN", evidence: "Step 13 stages 0-50 PASS; scenario E breach", scope: "Local", notes: "Stage-gated volume/windows; expected_stage guards workers" },
  { capability: "Write quiesce", status: "PROVEN", evidence: "Step 14 (503 on write); quiesce_test.go", scope: "Local", notes: "Ownership stays aws while writes pause" },
  { capability: "Ownership transfer", status: "PROVEN", evidence: "Step 17 OWNERSHIP_TRANSFERRED; scenario J exactly-once", scope: "Local", notes: "CAS commits once; source flips first, target second" },
  { capability: "Cutover", status: "PROVEN", evidence: "Step 17 CUTOVER_COMPLETE, 9 recorded stages", scope: "Local", notes: "Preflight revalidates everything; resumable blocks" },
  { capability: "Azure write / AWS reject", status: "PROVEN", evidence: "Step 18 AZURE_WRITE_OK AWS_REJECTED_OK", scope: "Local", notes: "Per-request ownership enforcement in both shops" },
  { capability: "Split-brain prevention", status: "PROVEN", evidence: "Step 18 agreement check; TestSplitBrainInvariant", scope: "Local", notes: "Ordered flips + enforcement; never dual-authoritative" },
  { capability: "Recovery / resume", status: "PARTIALLY PROVEN", evidence: "cutover_recovery_test.go; scenarios K-L", scope: "Local", notes: "Resume paths unit-proven; live mid-cutover restart not forced" },
  { capability: "Idempotency", status: "PROVEN", evidence: "SHA-256 request_hash, 409 conflicts, replays; scenario J", scope: "Local", notes: "Semantic replay; 24h expiry" },
  { capability: "Observability / evidence", status: "PROVEN", evidence: "Audit trail, cutover.json, status command", scope: "Local", notes: "Every transition audited with evidence hashes" },
  { capability: "Failure matrix A-L", status: "PROVEN", evidence: "FAILURE_MATRIX = COMPLETE, 10/10 RESULT=PASS", scope: "Local", notes: "Live where deterministic, unit proofs otherwise" },
  { capability: "Deterministic reset", status: "PROVEN", evidence: "RESET_OK; repeated same-RUN_ID runs", scope: "Local", notes: "AWS authoritative restored; demo rows removed" },
  { capability: "Temporal execution", status: "PARTIALLY PROVEN", evidence: "SDK workflows, mock-only activities; fail-closed without TEMPORAL_HOST", scope: "Local", notes: "Skeleton only; full-lifecycle orchestration deferred" },
  { capability: "Terraform modules", status: "PARTIALLY PROVEN", evidence: "14 modules fmt/validate green via tf-plan.sh", scope: "Local", notes: "plan needs creds (CI OIDC); apply never runs" },
  { capability: "Real AWS integration", status: "DEFERRED", evidence: "Fail-closed (DescribeLiveEnvironment refuses)", scope: "Real", notes: "Intentionally blocked; zero calls made" },
  { capability: "Real Azure", status: "DEFERRED", evidence: "Untouched", scope: "Real", notes: "Zero calls made" },
  { capability: "Front Door routing", status: "DEFERRED", evidence: "Local routing flag only", scope: "Real", notes: "Cutover switches local routing; no Front Door runs" },
  { capability: "Reverse CDC", status: "DEFERRED", evidence: "-", scope: "Real", notes: "Required before any post-authority rollback" },
  { capability: "Post-authority rollback", status: "DEFERRED", evidence: "Rejected by design (POST_WRITE_ROLLBACK_BLOCKED)", scope: "-", notes: "Resume-forward only in v1" },
  { capability: "Source teardown", status: "DEFERRED", evidence: "Never automatic in v1", scope: "-", notes: "By design" },
  { capability: "Console UI", status: "PROVEN", evidence: "This console: read-only Next.js views over live control-plane APIs + acceptance-console.sh", scope: "Local", notes: "Observability layer only; no mutation controls" },
  { capability: "AI planner/validator (advisory)", status: "PROVEN", evidence: "Mock-provider reviews over live control-plane evidence + acceptance-ai.sh", scope: "Local", notes: "Advisory only (NEVER_BY_AI); no authorization/mutation path" },
  { capability: "Real-LLM review path", status: "DEFERRED", evidence: "Optional HTTP provider, unexercised", scope: "Real", notes: "Experimental; never required" },
  { capability: "Production RPO/RTO", status: "DEFERRED", evidence: "Local lag measured only", scope: "Real", notes: "Never claimed" },
];

export const EVIDENCE_BOUNDARY = {
  localOnly: "Local demo environment: PostgreSQL + Debezium + Redpanda + control plane on one machine.",
  realAws: "Real AWS status: DISABLED / BLOCKED. No credentials loaded, no calls made, no spending.",
  realAzure: "Real Azure status: DISABLED / BLOCKED. Untouched, zero calls made.",
  terraformApply: "Terraform Apply status: DISABLED. Modules are fmt/validate only; apply never runs.",
  noMutation: "No real cloud mutation. No cloud spending. Local demo environment.",
};
