# AI Planner / Validator (advisory only)

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-29
>
> AI MAY RECOMMEND. AI MAY EXPLAIN. AI MAY IDENTIFY RISKS.
> AI MUST NEVER AUTHORIZE OR EXECUTE.

## 1. AI purpose

The AI planner/validator (`apps/agent-service/skybridge_ai`) helps an
operator UNDERSTAND and REVIEW a proposed migration. It summarizes
authoritative control-plane evidence, identifies risks, and suggests
checks. It is strictly advisory: the deterministic control plane
remains the sole authority for compatibility, planning, drift, policy,
approval, execution, ownership, and cutover.

No production AI reliability is claimed. The exercised model path is a
deterministic rule-based mock; any real LLM wiring is experimental,
optional, and never required.

## 2. Architecture

```text
Operator / Console (GET only)
  │
  │  POST /v1/ai/migration-review {workload_id, migration_id, target_provider}
  │  GET  /v1/ai/migration-review?migration_id=...&target_provider=azure
  ▼
AI service (apps/agent-service, stdlib HTTP, port 18082 default)
  │  1. fetch evidence      — control-plane GET endpoints ONLY
  │  2. sanitize            — secret stripping, untrusted-content framing
  │  3. fingerprint         — sha256 over canonical evidence JSON
  │  4. prompt (ai-planner-v1) + evidence -> model provider
  │  5. validate            — schema, NEVER_BY_AI, evidence grounding
  ▼
Envelope {review | null, prompt_version, model_id, evidence_fingerprint,
          evidence[], validation_result, error}
```

Modules: `config.py` (env-only config), `evidence.py` (GET-only
retrieval + fingerprint), `sanitize.py` (secret stripping),
`schema.py` (output contract, `ai-planner-v1`), `provider.py`
(deterministic mock + optional HTTP LLM + timeout wrapper),
`validate.py` (fail-safe response validation), `review.py`
(orchestration), `server.py` (stdlib HTTP server, no new dependencies).

## 3. Inputs (authoritative evidence only)

The model context is built ONLY from control-plane GET responses:

- canonical workload specification (`GET /v1/workloads/{id}`)
- migration record (`GET /v1/migrations/{id}`)
- compatibility report (`.../compatibility`)
- migration plan (`.../plan`)
- drift report (`.../drift`, latest item)
- approval status (`.../approvals`, bounded to 20)
- canary history (`GET /v1/migrations/{id}/canary`)
- cutover status (`.../cutover`)
- rehearsal result, CDC lag/positions, reconciliation, ownership
  (via `.../summary` + audit tail, bounded to 25 entries)
- static known limitations (local-only boundary)

Missing evidence stays missing and is reported under
`missing_evidence`. The model never invents values. Every evidence
item carries `{id, source, version, timestamp}` (see
`evidence_references()`).

## 4. Outputs (structured, never prose-first)

```json
{
  "migration_id": "...",
  "assessment": {
    "summary": "...",
    "risks": [{"statement": "...", "evidence_refs": ["drift:..."]}],
    "warnings": [{"statement": "...", "evidence_refs": ["compat:..."]}],
    "missing_evidence": [],
    "recommended_checks": [],
    "evidence_references": []
  },
  "proposed_plan_changes": [],
  "confidence": "low|medium|high",
  "authorization": "NEVER_BY_AI"
}
```

`authorization` is always the literal `NEVER_BY_AI`. No `decision`
field with an allow/deny/approve/execute value may appear; the
validator rejects it (`forbidden_authoritative_decision`).

## 5. Evidence grounding

Before the model call, evidence is canonicalized (sorted-key JSON)
and fingerprinted (`sha256:...`, recorded on the envelope). The model
receives stable IDs (report/plan/approval/canary/audit-request IDs,
planner + compatibility versions, policy input hash via approvals and
cutover metadata, CDC positions). Every risk and warning must cite at
least one supplied evidence id; every cited id must exist in the
bundle, otherwise the response is rejected as
`fabricated_evidence_reference`. Reproducibility: same snapshot +
deterministic provider -> same statements (natural-language text
equality is NOT required, schema + references + safety are).

## 6. Prompt / versioning

System prompt version `ai-planner-v1` (`build_system_prompt()`).
Each envelope records `prompt_version`, `model_id`,
`evidence_fingerprint`, `request_id`, `migration_id`, and
`validation_result`. Request/response bodies and API keys are never
logged (the server suppresses access logging of bodies).

## 7. Security

- No credentials, tokens, keys, passwords, or connection strings are
  sent to the model: `sanitize()` redacts secret-like keys before
  prompt construction (regression-tested).
- Only the minimum evidence required is included (bounded audit tail
  and approval list; no raw database rows or application payloads).
- Config is env-only (`SKYBRIDGE_AI_PROVIDER=mock` default,
  `SKYBRIDGE_AI_MODEL_ID`, `SKYBRIDGE_AI_TIMEOUT_SECONDS`,
  optional `SKYBRIDGE_AI_ENDPOINT` / `SKYBRIDGE_AI_API_KEY` for the
  experimental HTTP provider). Nothing is hardcoded.

## 8. Failure behavior (fail-safe)

| Condition | Result |
|---|---|
| Unknown workload/migration | 404 envelope, `review: null` |
| Control plane unreachable | 502 envelope, `review: null` |
| Model timeout | 504 envelope, `review: null` |
| Provider error | 502 envelope, `review: null` |
| Malformed/invalid model output | 502 envelope, `review: null` |
| migration_id mismatch | 502 envelope, `review: null` |

The service never returns a fabricated review and never implies
success: the console renders "AI review unavailable" with UNKNOWN
semantics when the service is down.

## 9. Prompt-injection handling

Workload/application data is UNTRUSTED CONTENT: it is wrapped in
`BEGIN/END UNTRUSTED` delimiters with an explicit ignore-instructions
directive, and the validator independently rejects any authoritative
decision the model might emit. Injection strings in workload names,
drift finding text, or audit actor fields cannot change
control-plane rules (regression-tested, including a hostile-model
case). AI output is untrusted input and is never fed into
authorization.

## 10. Boundary: AI vs policy vs execution

```text
AI:               observe, summarize, explain, identify risks, suggest checks
CONTROL PLANE:    compatibility, planning, drift, policy, approval,
                  execution, ownership, cutover
```

The AI result is NEVER fed into authorization: no code path passes a
review into policy evaluation, approval decisions, ownership
transfer, traffic switching, quiesce, Terraform, cloud APIs, or
rollback (enforced by `AI_NO_AUTHORIZATION_PATH` /
`AI_NO_MUTATION_PATH` acceptance guards plus a live
audit-trail-unchanged check). The advisory review itself performs no
mutation: POST review leaves the audit trail byte-identical.

## 11. Cost / cloud safety

FREE and LOCAL: mock provider only for tests/acceptance, no AWS/Azure
calls, no Terraform apply/destroy, no cloud credentials, no paid APIs
required. `ApplyEnabled=false` untouched.

## 12. Verification

`./scripts/acceptance-ai.sh` ends `SKYBRIDGE_AI_ADVISORY = PASS`.
Unit suite: `apps/agent-service/tests/` (40 tests).
