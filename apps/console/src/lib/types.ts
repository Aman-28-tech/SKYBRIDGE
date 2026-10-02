export type LifecycleStage = {
  name: string;
  status: "completed" | "current" | "pending";
  timestamp?: string;
  detail?: string;
};

export type CutoverStage = {
  name: string;
  status: "completed" | "current" | "pending" | "blocked";
  timestamp?: string;
  request_id?: string;
  actor?: string;
  policy_hash?: string;
  approval_id?: string;
  source_lsn?: string;
  applied_lsn?: string;
  lag_seconds?: number | null;
  detail?: string;
};

export type CdcView = {
  source_lsn?: string;
  applied_lsn?: string;
  lag_seconds?: number | null;
  rpo_seconds?: number;
  events_captured?: number | null;
  events_applied?: number | null;
  events_duplicates?: number | null;
  reconciliation_match?: boolean | null;
  reconciliation_fingerprint?: string;
  within_rpo?: boolean | null;
};

export type OwnershipView = {
  current_owner: string;
  previous_owner?: string;
  routing: string;
  source_writable: boolean;
  target_writable: boolean;
  split_brain: string;
  source_explanation?: string;
  target_explanation?: string;
};

export type SafetyView = {
  compatibility?: string;
  drift_gate?: string;
  drift_severity?: string;
  policy_decision?: string;
  approval?: string;
  canary_verdict?: string;
  canary_stage?: number;
  quiesce?: string;
  rollback?: string;
  split_brain?: string;
};

export type SummaryResponse = {
  migration_id: string;
  workload_id: string;
  migration?: Record<string, unknown>;
  workload?: { id: string; name: string };
  providers?: {
    source_provider: string;
    target_provider: string;
    authoritative_provider: string;
    routing_provider: string;
  };
  mode?: {
    local_demo: boolean;
    real_aws: string;
    real_azure: string;
    terraform_apply: string;
  };
  lifecycle?: { current: string; stages: LifecycleStage[] };
  cutover?: {
    status: string;
    current_stage: string;
    stages: CutoverStage[];
  };
  cdc?: CdcView;
  ownership?: OwnershipView;
  safety?: SafetyView;
  request_id?: string;
};

export type AuditItem = {
  run_id: string;
  workload_id: string;
  actor_type: string;
  actor_id: string;
  action: string;
  resource: string;
  request_id: string;
  idempotency_key: string;
  policy_bundle_version: string;
  policy_decision: string;
  approval_id: string;
  result: string;
  metadata: string;
  created_at: string;
};

export type AiRiskOrWarning = {
  statement: string;
  evidence_refs: string[];
};

export type AiReview = {
  migration_id: string;
  assessment: {
    summary: string;
    risks: AiRiskOrWarning[];
    warnings: AiRiskOrWarning[];
    missing_evidence: string[];
    recommended_checks: string[];
    evidence_references: string[];
  };
  proposed_plan_changes: unknown[];
  confidence: "low" | "medium" | "high";
  authorization: "NEVER_BY_AI";
};

export type AiEvidenceRef = {
  id: string;
  source: string;
  version?: string;
  timestamp?: string | null;
};

export type AiReviewEnvelope = {
  request_id?: string;
  migration_id: string;
  workload_id: string;
  target_provider: string;
  review: AiReview | null;
  prompt_version?: string;
  model_id?: string;
  evidence_fingerprint?: string | null;
  evidence?: AiEvidenceRef[];
  validation_result?: string;
  error?: string | null;
};

export type FetchState<T> = {
  data: T | null;
  error: string | null;
  loading: boolean;
  updatedAt: number | null;
  stale: boolean;
};

export const LIFECYCLE_ORDER = [
  "REGISTERED",
  "COMPATIBILITY",
  "PLANNED",
  "REHEARSED",
  "CANARY",
  "QUIESCED",
  "CUTOVER",
  "COMPLETE",
];

export const CUTOVER_ORDER = [
  "FINAL_PREFLIGHT",
  "WRITES_QUIESCED",
  "CDC_CATCHING_UP",
  "CDC_CAUGHT_UP",
  "FINAL_VALIDATION",
  "OWNERSHIP_TRANSFERRED",
  "TRAFFIC_SWITCHED",
  "WRITES_RESUMED",
  "CUTOVER_COMPLETE",
];
