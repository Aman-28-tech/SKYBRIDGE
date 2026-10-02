-- Target migration plans (planning ONLY). Derived, deterministic documents:
-- plan id is a deterministic function of (workload, compatibility report,
-- registry version, planner version, spec), so repeat generations upsert.
CREATE TABLE IF NOT EXISTS target_migration_plans (
  id UUID PRIMARY KEY,
  workload_id UUID NOT NULL REFERENCES workloads(id),
  compatibility_report_id UUID NOT NULL,
  target_provider TEXT NOT NULL,
  overall_status TEXT NOT NULL,
  registry_version INTEGER NOT NULL,
  planner_version TEXT NOT NULL,
  plan JSONB NOT NULL DEFAULT '{}',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_plan_workload ON target_migration_plans (workload_id, created_at DESC);
