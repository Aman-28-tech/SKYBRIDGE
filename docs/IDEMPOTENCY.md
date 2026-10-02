# Control-Plane Idempotency Contract

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-25

Authoritative rule: `docs/CLOUDSHOP_API.md` (canonical idempotency). This file records the control-plane-specific decisions.

## Rule

For every side-effecting control-plane mutation (`POST /v1/workloads`, `POST /v1/migrations`):

- `Idempotency-Key` header mandatory (16–128 chars).
- Fingerprint: `request_hash = hex(SHA-256(canonical JSON request body))`, where canonical JSON is the re-marshal of the decoded body (object keys sorted).
- Same key + same body → replay the stored response: **same HTTP status and semantically equal JSON body** (parsed-document deep-equal), no new resource. Byte-for-byte identity is NOT required: the PG stores keep the payload in a JSONB column, and PostgreSQL normalizes JSONB serialization on read (whitespace separators, key ordering), so the replayed body is semantically identical but textually different from the original bytes. Clients MUST parse the JSON, never compare raw bytes or Content-Length. (An earlier slice note claimed byte-identical replay; that claim was wrong and is corrected here.)
- Replay returns the response recorded at acceptance time, even if the underlying resource has since progressed (e.g. an execution that was `running` at acceptance and later `succeeded` still replays the original `202 running` payload with the same workflow/run identity). This is intentional: replay proves *no second execution happened*. Current state is always read from the resource's GET endpoint (`GET /v1/migrations/{id}/execution`), never from a replay. Returning live state on replay would conflate the write and read paths and make retries indistinguishable from re-execution.
- Same key + different body → `409` with `code: IDEMPOTENCY_CONFLICT`, no resource created/updated.
- Concurrent same-key submissions → one winner; losers get `409`. Serialized per-key in-process (see below).
- Retention: `expires_at = created_at + 24h`. Reuse after expiry is a new request. No janitor yet (expired rows ignored on read; add scheduled delete in Phase 12).

## Scope

Control-plane keys live in a single namespace (`idempotency_records.key`); there is no `(scope, key)` split as in CloudShop. Scope is implicit per operation endpoint. Clients MUST use distinct keys per operation. (Accepted to preserve API compatibility with existing keys.)

## Audit on conflict (decision)

A `409 IDEMPOTENCY_CONFLICT` **is audited** with `result=denied`, `action` set to the attempted mutation, `policy_decision` unset (no policy evaluated), and the key in `idempotency_key`. Rationale: FR-014 requires an audit trail for every mutation outcome, and `DOMAIN_MODEL.md` explicitly includes `denied` in the result enum — consistent with how policy-denied cutovers are already audited.

## Validation failures (decision)

A request rejected by schema validation (`400 VALIDATION_FAILED`) creates **no audit event**, saves **no idempotency record**, and persists **nothing**. Rationale: consistent with the pre-existing malformed-JSON path — no mutation was attempted and no policy evaluated, so there is no mutation outcome to trail. Processing order is decode → schema validation → idempotency → persistence, which guarantees an invalid request can never poison a later valid retry under the same key.

## Legacy rows

`request_hash` is nullable (migration `002_idempotency_hash.sql`). Rows written before this slice have `NULL` hash and replay without conflict comparison. No existing records were deleted or modified.

## Concurrency

Single control-plane instance (v1): a per-key in-process mutex covers check → create → save. The PG primary key on `idempotency_records` is the cross-instance backstop (`ON CONFLICT DO NOTHING`). Multi-instance deployments (Phase 12) need advisory-lock or reservation semantics before claiming duplicate-safety across replicas.

## Out of scope for this slice

Cutover/rollback endpoints accept `Idempotency-Key` (OpenAPI) but do not yet enforce replay/conflict semantics — tracked as a limitation, not changed here.
