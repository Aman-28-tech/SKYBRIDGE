# Runbook — Drift Response

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-25

## Purpose

Triage desired-vs-observed drift by severity with no auto-remediation.

## Preconditions

- [ ] Drift report ID, severity (`informational|blocking|security_critical`), desired/observed refs, diff captured

## Commands (placeholders)

```bash
curl /api/v1/workloads/<id>/drift?status=open
# acknowledge (requires operator role; security_critical pages immediately)
```

## Verification

- [ ] `informational`: logged, no gate impact
- [ ] `blocking`: `READY_FOR_CUTOVER` barred until `acknowledged`→resolved (fix forward in Terraform or update desired via reviewed PR)
- [ ] `security_critical`: alert fired, migration barred, exposure revoked first (close public access, scope-down IAM) before any migration resumes

## Failure symptoms

- Drift flap (re-detected after resolve) → discovery normalization bug or out-of-band automation fighting Terraform; find the writer

## Rollback/recovery

- No auto-remediation in v1. Remediation = reviewed change (Terraform apply or desired-model PR), never blind re-apply of observed state

## Cleanup

- [ ] Report transitioned to `resolved` with fixing commit/apply ID; audit retained

## Owner

Control-plane operator; security on-call for `security_critical`.
