# ADR — JSON Schema Validation Library (control-plane: santhosh-tekuri/jsonschema v6.0.3)

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-25

Per `docs/TECH_STACK.md` deferred-technology rule.

## Problem

FR-004 requires workload registration to yield a schema-valid canonical model. Hand-rolled field checks cannot enforce the canonical schema's types, enums, nested structure, numeric/string constraints, or `additionalProperties` behavior.

## Options

- `santhosh-tekuri/jsonschema/v6` (draft 2020-12, pure Go, no cgo)
- `qri-io/jsonschema` (draft-07 only — cannot evaluate our 2020-12 schema)
- Hand-written validators (duplicates the schema; drifts; rejected by slice requirements)
- OPA for structural validation (wrong layer; Rego stays for policy gates)

## Decision

`github.com/santhosh-tekuri/jsonschema/v6 v6.0.3` in `apps/control-plane` only, pinned in `go.mod`/`go.sum`.

## Why

Only maintained pure-Go option supporting draft 2020-12 (`const`, `pattern`, `additionalProperties` all verified against our schema). No cgo, so static Alpine images keep working.

## Trade-offs / operational cost

One more module to audit on upgrade; schema compiled once at startup (`sync.Once`) so per-request cost is validation only. The schema file is embedded via a byte-identical copy (`apps/control-plane/schemas/`) because `go:embed` cannot cross the module boundary — a sync test fails the build on drift, so the canonical file remains the single source of truth.

## Test plan

`validate_test.go`: sync test, valid/invalid matrices (required, type, const, minimum, pattern, root+nested additionalProperties), handler-level 400/details/no-state/no-poisoning, idempotency preservation, opt-in PG-backed test. `go test -race ./...` green.

## Evidence

Implementation validation: this slice's test run (22/22 with `-race`).
