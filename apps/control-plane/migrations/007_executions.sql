-- Execution records (Temporal-backed stage executions).
-- Bookkeeping attached to the Migration model (migration_id FK), not a
-- second lifecycle: workflow identity is deterministic, so the workflow_id
-- unique constraint doubles as duplicate-execution prevention.
CREATE TABLE IF NOT EXISTS executions (
  id UUID PRIMARY KEY,
  workflow_id TEXT NOT NULL UNIQUE,
  run_id TEXT NOT NULL DEFAULT '',
  migration_id UUID NOT NULL REFERENCES migrations(id),
  workload_id UUID NOT NULL REFERENCES workloads(id),
  target_weight INTEGER NOT NULL,
  status TEXT NOT NULL DEFAULT 'accepted',
  policy_input_hash TEXT NOT NULL DEFAULT '',
  approval_id UUID NULL,
  error TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_exec_migration ON executions (migration_id, created_at DESC);
