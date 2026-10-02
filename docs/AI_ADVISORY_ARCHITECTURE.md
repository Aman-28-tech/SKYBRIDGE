# Agentic AI Architecture

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 2
> Last updated: 2026-09-25

## Roles (all proposal-only; never authorization)

### Discovery Analyst

Explains the normalized workload and missing evidence. Input: discovery snapshots + canonical draft. Output: structured gaps list with evidence IDs.

### Migration Planner

Creates a structured migration plan validating against `MigrationPlan` schema (see DOMAIN_MODEL.md). Must cite `requirements.rpo_seconds=30`, `rto_seconds=900`, capability checks, and drift state.

### Validation Analyst

Interprets validation failures (health, DB, queue, canary gates). Must reference observed metric IDs, never invent state.

### Recovery Planner

Proposes rollback/recovery steps respecting the v1 rule: pre-write rollback allowed, post-write traffic-only rollback blocked unless `reverse_sync_ready`. Must state `write_ownership` explicitly.

## Grounding format (mandatory)

Agents receive structured evidence references:

```json
{
  "evidence": [
    { "id": "metric-123", "type": "replication_status", "source": "skybridge", "observed_at": "2026-09-25T00:00:00Z", "run_id": "run_...", "workload_id": "wl_..." }
  ]
}
```

The agent must reference evidence IDs for every factual claim. A claim without an evidence ID is invalid output.

## Prompt/version management

Store for every agent run:

```text
model_id
prompt_name
prompt_version
tool_schema_version
policy_bundle_version
run_id
evidence IDs cited
```

## Planning output

Agent output must validate against its JSON schema (plan/validation/recovery). Natural-language explanation is supplemental, never a substitute.

## Agent execution boundary

```text
AI
 |
proposal (schema-validated, evidence-cited)
 |
authentication -> authorization -> OPA -> approval if required
 |
Temporal workflow
 |
provider adapter -> cloud -> verification
```

No direct arbitrary cloud execution. AI never becomes authorization.

## Evaluation

- plan schema validity
- constraint adherence (RPO/RTO/network/security/capabilities)
- evidence grounding (every claim cited)
- unsafe-action refusal (destructive/source-destroy/post-write-rollback without reverse sync)
- tool selection (read-only vs planning vs mutation discipline)
- recovery-plan usefulness (pre/post-write correctness, explicit ownership)
