-- Control-plane durable state (Postgres). Business data lives in CloudShop; this DB
-- owns workflow/audit state only.
CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE TABLE IF NOT EXISTS workloads (
  id UUID PRIMARY KEY,
  name TEXT NOT NULL,
  schema_version INTEGER NOT NULL,
  lifecycle_state TEXT NOT NULL,
  canonical_spec JSONB NOT NULL DEFAULT '{}',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS migrations (
  id UUID PRIMARY KEY,
  workload_id UUID NOT NULL REFERENCES workloads(id),
  status TEXT NOT NULL DEFAULT 'REGISTERED',
  current_step TEXT NOT NULL DEFAULT 'registered',
  request_id TEXT,
  idempotency_key TEXT,
  policy_bundle_version TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  completed_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS idempotency_records (
  key TEXT PRIMARY KEY,
  response JSONB NOT NULL,
  status INTEGER NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '24 hours'
);

-- AuditEvent per DOMAIN_MODEL.md (operational store; high-value rows also exported).
CREATE TABLE IF NOT EXISTS audit_events (
  id UUID PRIMARY KEY,
  run_id UUID,
  workload_id UUID,
  actor_type TEXT NOT NULL,
  actor_id TEXT NOT NULL,
  action TEXT NOT NULL,
  resource TEXT NOT NULL DEFAULT '',
  request_id TEXT,
  idempotency_key TEXT,
  policy_bundle_version TEXT,
  policy_decision TEXT,
  approval_id UUID,
  result TEXT NOT NULL,
  metadata JSONB NOT NULL DEFAULT '{}',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_audit_run ON audit_events (run_id);
