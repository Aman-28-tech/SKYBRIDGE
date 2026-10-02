-- H-2 object-level authorization: record the verified registering principal
-- on each workload. Only the owner (or an admin principal) may mutate it.
-- Nullable so pre-existing rows upgrade without deletion; rows without an
-- owner are admin-only (fail-closed). Also ensured at connect time by
-- NewPGStore (ALTER ... ADD COLUMN IF NOT EXISTS) for pre-existing volumes.
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS owner_id TEXT;
