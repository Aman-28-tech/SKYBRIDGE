-- SKYBRIDGE CDC durable event dedupe (Option A).
--
-- SEMANTICS (must read before changing):
--   cdc_applied_events(event_id) is the AUTHORITATIVE per-event dedupe marker.
--   cdc_applied_txn(txn_id) is an observability/completion marker only and is
--   NEVER consulted to skip an event. One PostgreSQL transaction can emit many
--   row events (T1 -> E1,E2,E3); skipping on txn_id after E1 would corrupt E2/E3.
--   cdc_offsets(consumer) checkpoint advances only past safely applied LSNs.
CREATE TABLE IF NOT EXISTS cdc_applied_events (
  event_id TEXT PRIMARY KEY,
  txn_id TEXT NOT NULL DEFAULT '',
  tbl TEXT NOT NULL DEFAULT '',
  source_lsn TEXT NOT NULL DEFAULT '',
  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
