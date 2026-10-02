-- Compatibility reports (FR-005). Derived, deterministic cache of evaluations:
-- report id is a deterministic function of (workload, registry version,
-- evaluator version, spec), so repeat evaluations upsert to the same row.
CREATE TABLE IF NOT EXISTS compatibility_reports (
  id UUID PRIMARY KEY,
  workload_id UUID NOT NULL REFERENCES workloads(id),
  target_provider TEXT NOT NULL,
  status TEXT NOT NULL,
  registry_version INTEGER NOT NULL,
  evaluator_version TEXT NOT NULL,
  checks JSONB NOT NULL DEFAULT '[]',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_compat_workload ON compatibility_reports (workload_id, created_at DESC);
