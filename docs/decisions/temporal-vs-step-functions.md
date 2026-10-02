# ADR — Temporal vs Step Functions (v1: Temporal)

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-25

## Context

Migration runs for hours/days across AWS + Azure, needs durable state, retries, timeouts, restart recovery, and provider-neutral orchestration. Candidates: Temporal, AWS Step Functions, Argo Workflows, custom state machine.

## Options

- Temporal (self-hosted, provider-neutral durable workflows)
- AWS Step Functions (managed, AWS-bound orchestration)
- Argo Workflows (K8s-native, cluster-bound)
- Custom workflow state machine (full control, full burden)

## Decision

Temporal for v1 durable workflow engine. It owns provider-neutral migration state; cloud workflow services may be added later as provider-specific adapters, never as the v1 state owner.

## Why

Provider-neutral (AWS+Azure in one workflow), durable timers/retries/replay, local development (docker), explicit failure/recovery states matching `WORKFLOW_STATE_MACHINE.md`.

## Trade-offs

Adds a platform to operate (server + DB + worker scaling). Learning curve for workflows/activities/versioning. Local Temporal ≠ managed-service behavior — milestone validation still needs real runs.

## Consequences

`apps/control-plane` + Temporal workers own orchestration; adapters own provider calls; `RETRYING/PAUSED/FAILED/RECOVERY` semantics per state machine; chaos tests cover worker restart/retry/timeout/duplicate.

## Evidence

Pending implementation validation (Phase 8 worker-restart resume + retry/timeout tests).
