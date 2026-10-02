-- Approvals (explicit human authorization for policy-gated actions).
-- No approvals table existed before: audit_events only referenced approval_id.
-- One row per approval request; decisions are compare-and-set pending -> X.
-- Expiry is temporal (expires_at), not a decision state: the domain decision
-- enum stays pending|approved|rejected; expired pendings are unusable.
CREATE TABLE IF NOT EXISTS approvals (
  id UUID PRIMARY KEY,
  workload_id UUID NOT NULL REFERENCES workloads(id),
  migration_id UUID NULL,
  action TEXT NOT NULL,
  risk TEXT NOT NULL,
  decision TEXT NOT NULL DEFAULT 'pending',
  requested_by TEXT NOT NULL,
  requested_by_type TEXT NOT NULL DEFAULT 'human',
  decided_by TEXT,
  decided_by_type TEXT,
  policy_bundle_version TEXT,
  policy_input_hash TEXT NOT NULL,
  target_provider TEXT NOT NULL,
  plan_id UUID,
  compatibility_report_id UUID,
  drift_report_id UUID,
  target_weight INTEGER,
  environment TEXT NOT NULL DEFAULT 'dev',
  expires_at TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '24 hours',
  decided_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_approval_workload ON approvals (workload_id, created_at DESC);
