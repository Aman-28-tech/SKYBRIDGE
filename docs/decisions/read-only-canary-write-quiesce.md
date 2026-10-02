# ADR — Read-Only Canary + Write Quiesce (v1 cutover safety)

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-25

## Context

Unidirectional CDC (AWS → Azure) cannot safely support "route traffic back" after Azure takes writes — AWS would be stale. Leaving this as "safe write strategy" is not a design.

## Options

- Read-only canary + write quiesce (chosen): stages 1–4 read-only, Stage 5 transfers ownership via quiesce sequence
- Dual-write (rejected for v1: conflict resolution + ordering burden, untestable at beginner scope)
- Reverse CDC from day one (deferred: doubles replication work before first migration works)

## Decision

v1: AWS stays authoritative writer through stages 1–4 (Azure read-only shadow; writes to non-owner → `403 WRITE_NOT_OWNED`). Stage 5 follows stop-writes → drain → CDC catch-up (`cdc_lag_seconds <= 30`) → verify → transfer ownership → switch → verify → resume. Post-write traffic-only rollback is BLOCKED (`409 POST_WRITE_ROLLBACK_BLOCKED`) unless reverse sync is implemented and tested; use forward-fix instead.

## Why

Buildable and safe at beginner scope: no conflict resolution, no reverse pipeline, explicit ownership bit, testable gates. Matches CloudShop semantics (reads canaried, writes quiesced).

## Trade-offs

Brief write unavailability during quiesce (RTO budget 15 min must cover it). No "instant back" after ownership transfer — operationally honest but less forgiving. Reverse migration is a Phase 13 extension, not v1.

## Consequences

Enforced in `cutover.rego` (`read_only_canary`, `write_ownership`, `reverse_sync_ready`), state machine (`READY_FOR_CUTOVER` requires `ownership=aws`), CloudShop API guard, and rollback procedure (pre-write allowed / post-write blocked).

## Evidence

Pending implementation validation (rehearsal: write-to-non-owner 403 test + quiesce timing + post-write rollback-blocked test).
