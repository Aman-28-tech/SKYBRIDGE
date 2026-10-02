-- Strict idempotency: store the SHA-256 request fingerprint alongside the response.
-- Nullable so pre-existing rows upgrade without deletion: NULL hash means
-- "legacy record" and replays without conflict comparison (see docs/IDEMPOTENCY.md).
ALTER TABLE idempotency_records ADD COLUMN IF NOT EXISTS request_hash TEXT;
