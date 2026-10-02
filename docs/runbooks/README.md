# Runbooks

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 2
> Last updated: 2026-09-25

Runbooks are operational procedures, not architecture explanations. Each contains purpose, preconditions, commands/placeholders, verification, failure symptoms, rollback/recovery, cleanup, owner. Do not invent unverified cloud commands.

- `first-cloud-experiment.md` — first ephemeral scope with budget + cleanup
- `migration-rehearsal.md` — ephemeral practice without prod traffic
- `cutover.md` — canonical stages 0/1/5/25/50/100 + gates
- `rollback.md` — pre-write allowed / post-write blocked
- `cdc-recovery.md` — resume from durable offset without double-apply
- `drift-response.md` — severity triage, no auto-remediation
- `credential-compromise.md` — contain, rotate, preserve audit
- `observability.md` — run_id triage path (canonical; `../OBSERVABILITY_RUNBOOK.md` points here)
