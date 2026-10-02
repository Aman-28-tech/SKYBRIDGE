-- H-3 durable admin state: the single authoritative-writer fact plus the
-- quiesce flag for this CloudShop deployment. The running process persists
-- every admin mutation here BEFORE swapping memory (persist-then-swap) and
-- loads it at startup, so a process failure recovers to the last committed
-- state — never a resurrected second writer. Also ensured at connect time
-- by NewPGStore (CREATE TABLE IF NOT EXISTS) for pre-existing volumes.
CREATE TABLE IF NOT EXISTS admin_state (
  singleton INT PRIMARY KEY DEFAULT 1 CHECK (singleton = 1),
  write_ownership TEXT NOT NULL,
  quiesced BOOLEAN NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
