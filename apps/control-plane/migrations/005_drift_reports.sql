-- Drift reports (FR-017, detection only). Derived, deterministic evaluations:
-- report id is a deterministic state fingerprint of (workload, plan, snapshot),
-- so re-observing an identical state refreshes detected_at on the same row
-- (recency) instead of duplicating it; distinct states remain separate rows.
-- Findings live in diff JSONB.
CREATE TABLE IF NOT EXISTS drift_reports (
  id UUID PRIMARY KEY,
  workload_id UUID NOT NULL REFERENCES workloads(id),
  severity TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'open',
  desired_reference TEXT NOT NULL,
  observed_reference TEXT NOT NULL,
  diff JSONB NOT NULL DEFAULT '{}',
  detected_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_drift_workload ON drift_reports (workload_id, detected_at DESC);
