# ADR — Go Control Plane (v1: Go)

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-25

## Context

Control plane needs cloud API clients, concurrency, long-running workers, and infrastructure-grade reliability. Candidates: Go, Node/TypeScript, Java, Python.

## Options

- Go (static binary, concurrency, cloud SDKs, infra ecosystem)
- Node/TypeScript (console synergy, weaker long-running/adapter story)
- Java (heavy for a learning project)
- Python (agent-service already uses it; weaker for the serving control plane)

## Decision

Go for `apps/control-plane` (API + discovery + adapters + compatibility + drift + validation + orchestration glue). Python + LangGraph stays scoped to `apps/agent-service` (reasoning only). Next.js/TypeScript stays scoped to `apps/console`.

## Why

Strong fit for cloud APIs, concurrency, and infra tooling; distinct systems-engineering learning opportunity; clear technology boundary per `REPO_STRUCTURE.md`.

## Trade-offs

Builder has less Go experience (learning cost). Must enforce contract-first discipline so Go/Python/TS boundaries don't drift (OpenAPI + Proto + JSON Schema + Rego in CI).

## Consequences

Adapters (`adapters/aws`, `adapters/azure`) and Validator ship inside control-plane in v1; agent-service never touches cloud directly; console talks OpenAPI only.

## Evidence

Pending implementation validation (Phase 2 contract gates + Phase 6 discovery slice).
