# Runbook — Credential Compromise

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-25

## Purpose

Contain a suspected privileged-credential compromise without destroying evidence.

## Preconditions

- [ ] Suspect identity (role ARN / federated subject / JWT `actor_id`), scope, time window captured

## Commands (placeholders — confirm account/subscription before acting)

```bash
# 1. stop mutation workflows (pause, do not delete)
# 2. disable/remove trust (console/IaC PR; examples only — verify first)
aws iam delete-role-policy --role-name SKYBRIDGE-dev-terraform --policy-name allow || true
# 3. rotate (DB/JWT per SECURITY_MODEL periods; OIDC = update trust, no static secret)
```

## Verification

- [ ] No new mutations from suspect identity (audit query by `actor_id` + time window)
- [ ] Recent actions triaged (`success` vs `denied`; blast radius listed by scope/tfstate key)
- [ ] New trust/rotation verified with a read-only call before resuming

## Failure symptoms

- Continued denies after rotation → stale trust policy or cached token; check `aud`/`sub` and environment binding

## Rollback/recovery

- Restore normal operation only after verification + approval; retain full audit for review

## Cleanup

- [ ] Compromised material revoked (not just deleted locally), rotation recorded, post-mortem filed

## Owner

Security + platform. Page immediately for production-like scopes.
