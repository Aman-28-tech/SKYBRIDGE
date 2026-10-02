# Cloud Capability & Compatibility Engine

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 2
> Last updated: 2026-09-25

## Purpose

Do not migrate based on service-name similarity. Evaluate whether the target cloud can satisfy the workload's actual requirements.

## Input

- canonical workload (`schema_version`, `requirements.rpo_seconds=30`, `rto_seconds=900`)
- target provider + region
- capability registry (`packages/contracts/schemas/capability-registry.example.yaml`)
- security requirements
- capacity
- network requirements

## Output (canonical enum)

Overall and per-check status is exactly one of:

```text
pass | conditional | unknown | block
```

Aggregation (must match `DOMAIN_MODEL.md` and `cutover.rego`):

```text
if ANY check == block:       overall = block
else if ANY check == unknown: overall = unknown
else if ANY check == conditional: overall = conditional
else: overall = pass
```

Gate: only `overall == pass` enters automatic cutover readiness. `conditional` requires explicit human approval (policy `requires_approval`). `unknown` blocks automated migration. `block` blocks migration.

Each check contains:

```text
requirement
target capability
status (pass|conditional|unknown|block)
evidence (provider docs ref + test ref; missing evidence -> unknown, never pass)
assumptions
```

## Capability registry example

```yaml
capability:
  id: kubernetes.multi_zone
  provider: azure
  service: aks
  supported: true
  constraints:
    min_zones: 3
  evidence:
    - provider_documentation
    - integration_test
```

See: `packages/contracts/schemas/capability-registry.example.yaml`. CI validates the registry (schema + example). Required registry IDs for v1: `kubernetes.multi_zone`, `postgresql.private_networking`, `postgresql.extensions`, `network.private_connectivity`, `object.storage`, `queue.at_least_once`, `identity.workload`, `routing.weighted_canary`, `observability.metrics`.

## Unknown is first-class

If evidence is missing: `unknown`, not `pass`. Unknown blocks automated production-like cutover. A check without evidence is incomplete by definition.

## Semantic difference

Example: AWS VPC vs Azure VNet. The question is: can the target satisfy private connectivity, routes, security, DNS? Not: do names look equivalent? For every mapping (see `docs/AWS_AZURE_MAPPING.md`) document source capability, target capability, behavior difference, limitation, evidence, test status.
## No fake score

v1 does not produce a single numerical compatibility score.

A single score can hide a critical blocking constraint.

## Implementation (control-plane `compat.go`)

- Evaluator version `compat-v1`, recorded on every report alongside the registry version; identical (workload, registry version, evaluator version) inputs yield identical reports (deterministic report IDs).
- Evidence rule: integration-test evidence → `pass`; documentation-only → `conditional` (assumption recorded); missing entry or evidence → `unknown`; `supported: false` or violated hard requirement → `block`.
- Evaluation is analysis only: no provisioning, no mutation, no audit trail. Reports persist as derived records (`compatibility_reports`) for gate traceability.
- Endpoint: `POST /v1/workloads/{id}/compatibility` (202 + report), `GET` latest (200 or 404).