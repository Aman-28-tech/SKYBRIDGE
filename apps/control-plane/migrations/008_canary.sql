-- Canary stage history (deterministic staged evaluation per migration).
-- Records are append-only history; advancement rules live in canary.go
-- (expected-next enforcement + immutable PASS stages + per-migration lock).
CREATE TABLE IF NOT EXISTS canary_stages (
  id UUID PRIMARY KEY,
  migration_id UUID NOT NULL REFERENCES migrations(id),
  workload_id UUID NOT NULL REFERENCES workloads(id),
  stage INTEGER NOT NULL,
  verdict TEXT NOT NULL,
  baseline JSONB NOT NULL DEFAULT '{}',
  observed JSONB NOT NULL DEFAULT '{}',
  reasons JSONB NOT NULL DEFAULT '[]',
  request_id TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_canary_migration ON canary_stages (migration_id, created_at DESC);
