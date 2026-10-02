-- Write-ownership record: the single authoritative-owner fact per migration.
-- Absent row = AWS authoritative (v1 initial state). Only one row per
-- migration; TransferOwnership compare-and-swaps aws -> azure exactly once.
CREATE TABLE IF NOT EXISTS ownership (
  migration_id UUID PRIMARY KEY REFERENCES migrations(id),
  workload_id UUID NOT NULL REFERENCES workloads(id),
  current_owner TEXT NOT NULL,
  previous_owner TEXT NOT NULL DEFAULT 'aws',
  routing TEXT NOT NULL DEFAULT 'aws',
  policy_input_hash TEXT NOT NULL DEFAULT '',
  approval_id UUID NULL,
  source_lsn TEXT NOT NULL DEFAULT '',
  applied_lsn TEXT NOT NULL DEFAULT '',
  lag_seconds BIGINT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
