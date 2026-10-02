# ADR — OPA vs Cedar (v1: OPA/Rego)

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-25

## Context

Every privileged mutation needs testable, versioned, fail-closed policy. Candidates: OPA/Rego, Cedar, application if/else.

## Options

- OPA/Rego (general policy-as-code, bundles, `opa test`)
- Cedar (AWS-flavored authz, narrower ecosystem for migration gates)
- Application-only conditionals (no separate policy lifecycle)

## Decision

OPA/Rego. Policy bundle versioned, tested (`cutover.rego` + `cutover_test.rego`), evaluated fail-closed (unavailable → DENY + PAUSED).

## Why

Expressive gates (compat enum, drift counts, lag, weights, ownership), mature testing, bundle versioning recorded on every decision (`policy_bundle_version` + input hash).

## Trade-offs

Rego is another language/runtime to learn and operate (bundle distribution, version pinning, p95 budget). Cedar would be leaner for pure authz but weaker for multi-signal migration gates.

## Consequences

`packages/contracts/policy/` is authoritative; `POLICY_ENGINE.md` documents canonical inputs; CI runs `opa test`; every audit record carries bundle version + decision.

## Evidence

Pending implementation validation (policy latency p95 + deny/approval matrix tests).
