# SKYBRIDGE Security (public, v1 local)

> Scope: the FREE LOCAL v1 deployment. Local proof is not cloud proof.
> This document states the *current* security model. Production deployment
> needs the stronger controls listed under Known limitations.

## Authentication (local/dev)

- Every control-plane mutation requires `Authorization: Bearer <token>`
  (missing/invalid → `401 UNAUTHENTICATED`). Read-only `GET` endpoints
  stay open for the observability console.
- Tokens are verified server-side against operator-provided configuration
  (`SKYBRIDGE_LOCAL_TOKENS`, a JSON list mapping tokens to principals with
  `actor_id`, `actor_type`, and `admin`). There are no default tokens and
  no hardcoded secrets; an empty configuration fails closed (all mutations
  401).
- `X-Actor-Id` / `X-Actor-Type` request headers are **never trusted** for
  identity and are ignored entirely.
- CloudShop `/v1/admin/*` (quiesce, ownership) requires its own bearer
  token (`CLOUDSHOP_ADMIN_TOKENS`): no/invalid token → 401; valid
  non-admin token → 403 on mutations (reads allowed); admin token →
  allowed. The control plane forwards its service token on admin calls.
- This is **local/dev authentication, not production identity**. It
  provides verified caller identity inside the lab network, nothing more.

## Authorization

- Owner-or-admin per workload: registration records the verified
  `owner_id`; admins may act on any workload, anyone else only on
  workloads they registered.
- Strict migration→workload binding on every mutation: unknown migration
  → 404, workload mismatch → 404, authenticated-but-unauthorized → 403.
- Approval request/decide are workload-scoped; execution-time approvals
  are re-scoped to the calling workload (no cross-workload approval use,
  no cross-migration execution).

## Ownership protection (single writer)

- Exactly-once compare-and-swap transfer `aws → azure`; reversal refused.
- Commit-before-expose: the ownership fact commits before shop flips, so
  any interruption resumes forward onto the committed fact — never two
  writers, never a lost transfer. Shop flips run source-first (any
  interruption leaves a both-reject paused state).
- CloudShop admin state (ownership + quiesce) is durable (persist-then-swap,
  restored on startup); a restart can never resurrect a second writer.
- Post-authority rollback is blocked: after Azure owns writes,
  traffic-only rollback to AWS is rejected (no reverse CDC in v1;
  recovery is forward-fix from Azure state).

## Policy and approval

- Policy engine (Rego contract + Go mirror, fail-closed): every gated
  action evaluates stored evidence plus runtime context to
  `allow / deny / approval_required`. Missing or stale evidence denies.
- Human approval where required: human-only, distinct from the requester,
  bound to the exact policy-input hash, 24h expiry, revalidated for
  staleness at decision time and at use time.

## Idempotency

- `Idempotency-Key` + SHA-256 canonical-body fingerprint on side-effecting
  routes: same key + same body replays the stored response; same key +
  different body is a `409 IDEMPOTENCY_CONFLICT` (also audited).

## AI safety

- The AI planner/validator is advisory-only (`NEVER_BY_AI`): it summarizes
  authoritative evidence, cites evidence IDs, and suggests checks. Reviews
  that claim authority (`APPROVE`, `EXECUTE`, …) or lack evidence citations
  are rejected. There is no path from AI output into policy, approval,
  ownership, cutover, Terraform, or cloud APIs.

## Secret handling

- All SQL is parameterized; drift evidence, logs, and AI context strip
  secret-shaped values; committed-credential scanning runs in tests and CI.
- Demo credentials are local lab fixtures (per-run random tokens, local DB
  passwords); the repository contains no production secrets.

## Cloud safety boundary

- `ApplyEnabled=false` (compile-time constant) + adapter refusal + CI
  no-apply guard + boundary tests: Terraform never applies.
- No cloud SDKs in the mutation path; adapters fail closed without
  credentials. Verified: 0 real AWS/Azure calls, $0 spend.

## Audit

Every mutation records request ID, verified actor, approval reference,
policy bundle version, policy input hash, evidence IDs, CDC positions,
and timestamps — the full chain from registration to `CUTOVER_COMPLETE`
is reconstructable from the audit timeline.

## Known limitations (production-blocking, deferred)

Local/dev bearer auth is not production identity. Before any real-cloud
or internet exposure, v1 additionally needs: OIDC/mTLS, secret
management, TLS, request limits and timeouts, hardened hosting
(non-root, loopback-only, minimal binds), dependency upgrades, and a
fresh security review of the hardened system. The console stays
read-only; the AI stays advisory-only.
