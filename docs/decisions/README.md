# Architecture Decisions

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 2
> Last updated: 2026-09-25

Full decision records live here (each has Context/Options/Decision/Why/Trade-offs/Consequences/Evidence; pending evidence is stated explicitly):

- `temporal-vs-step-functions.md` — Temporal owns provider-neutral durable state
- `opa-vs-cedar.md` — OPA/Rego fail-closed policy
- `front-door-vs-dns-cutover.md` — Front Door Standard canary; DNS failover-only
- `kafka-vs-rabbitmq-for-cdc.md` — Kafka-compatible log for CDC (local Redpanda)
- `go-control-plane.md` — Go control plane; Python agent-service; TS console
- `read-only-canary-write-quiesce.md` — pre-write rollback allowed / post-write blocked
- `demo-safety-model.md` — implemented safety semantics: state, CDC, policy, approval, idempotency, ownership, split-brain, rollback, Apply boundary
