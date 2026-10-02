# API Contract Strategy

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 2
> Last updated: 2026-09-25

## Decision

Use:

- JSON Schema for canonical workload (`packages/contracts/schemas/canonical-workload.schema.json`, v1) + CloudShop fixture + capability-registry schema
- OpenAPI 3.1 for browser/control-plane HTTP (`packages/contracts/openapi/skybridge.yaml`, v1)
- Protobuf for migration/audit internal RPC/events (`migration.proto`, `audit.proto`; never reuse deleted field numbers)
- Rego for policy (`cutover.rego` + tests)

Executable contracts are authoritative over prose.

## Browser

```text
Next.js
 |
HTTP/JSON (OpenAPI, JWT bearer OIDC)
 |
Go Control Plane
```

## Error envelope (mandatory)

All control-plane AND CloudShop APIs MUST return on error:

```json
{
  "error": {
    "code": "VALIDATION_FAILED",
    "message": "Human-readable explanation",
    "request_id": "req_123",
    "details": {}
  }
}
```

Codes: see `docs/CLOUDSHOP_API.md` (shared set incl. `IDEMPOTENCY_CONFLICT`, `WRITE_NOT_OWNED`, `POST_WRITE_ROLLBACK_BLOCKED`).

## Pagination (mandatory)

List endpoints use `limit` (1–100, default 50) + opaque `cursor`. Response: `{items, next_cursor}` (`next_cursor: null` when done).

## Authentication / authorization

- Authentication: OIDC JWT bearer (`bearerAuth`). CI uses federated OIDC; runtime uses workload identity mapped to a service JWT; agents use internal service identity.
- Authorization: mutating endpoints check role/permission (`readonly|operator|approver`). `approver` required for production-like cutover, staging >25%, and `conditional` compat. Policy (OPA) + approval enforced server-side, never client-side.
- See `docs/SECURITY_MODEL.md` actor table.

## Idempotency

All POST/PUT/PATCH mutations require `Idempotency-Key` (16–128 chars), `SHA-256(body)` fingerprint, scope `workload+operation`, 24h retention. Same key+same body → replay; same key+different body → `409 IDEMPOTENCY_CONFLICT`. See `docs/CLOUDSHOP_API.md`.

## Internal contracts

Protobuf for migration workflow events, audit events, and typed internal RPC where gRPC is used. Every envelope carries `request_id`, `run_id`, `workload_id`, `schema_version` (+ `idempotency_key`/`policy_bundle_version` where applicable).

## Versioning

Breaking HTTP changes: `/v1 -> /v2` (old version supported ≥1 milestone or explicit sunset). Breaking schema: new major `schema_version`. Protobuf: compatible evolution only; never reuse field numbers.

## Contract checks

CI must validate (pinned versions in `.tool-versions`):

- JSON Schema parse + CloudShop fixture validation + capability-registry validation
- OpenAPI parse + lint
- Protobuf compilation (`buf` or `protoc`)
- Rego tests (`opa test`)

See `packages/contracts/` and `docs/CONTRACT_VALIDATION.md`. A broken contract fails CI; service code must not silently compensate.
