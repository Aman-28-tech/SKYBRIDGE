# Runbook — Rollback

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-25

## Purpose

Restore source traffic safely with explicit data semantics (pre-write allowed, post-write blocked).

## Preconditions

- [ ] Know `write_ownership` (aws = pre-write path; azure = post-write path)
- [ ] `run_id` + failing stage + evidence IDs captured

## Commands (placeholders)

```bash
curl -X POST /api/v1/migrations/<id>/rollback -H 'Idempotency-Key: <uuid>' -d '{"reason":"<stage-N gate breach>"}'
```

## Verification

Pre-write path:

- [ ] Azure weight 0, AWS 100 (desired + observed)
- [ ] AWS `/healthz` + `/readyz` 200, order flow works, no Azure writes occurred (audit confirms)
- [ ] Workflow state `ROLLED_BACK`, history retained

Post-write path (no reverse sync):

- [ ] Expect `409 POST_WRITE_ROLLBACK_BLOCKED`; DO NOT force traffic back (data loss)
- [ ] Switch to forward-fix: stabilize Azure, replay/reconcile, communicate; file reverse-sync extension if needed

## Failure symptoms

- Rollback workflow failure → `FAILED` + `RECOVERY`; preserve audit, page owner, never delete source to "clean up"

## Rollback/recovery of the rollback

- If rollback itself fails, hold traffic where it is provably consistent (usually AWS pre-write), re-verify ownership, escalate

## Cleanup

- [ ] Shadow Azure scope marked for later reconciliation/deletion (explicit approval); source never auto-destroyed

## Owner

Migration operator + approver. Source destroy is never automatic in v1.
