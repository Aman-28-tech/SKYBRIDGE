# ADR — Front Door vs DNS Cutover (v1: Front Door Standard)

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-25

## Context

Staged HTTP canary (1/5/25/50%) needs request-level splitting behind one hostname across AWS + Azure origins. Candidates: Azure Front Door (Standard/Premium), DNS weighting, ALB/Application Gateway alone.

## Options

- Front Door Standard (global L7, weighted origins, external origins, probes)
- Front Door Premium (Standard + Private Link/WAF; higher cost)
- DNS weighting (Route 53; response-level, TTL-cached)
- Per-cloud LBs only (no single cross-cloud weighted endpoint)

## Decision

Front Door Standard for v1. Premium reserved for Phase 12 hardening (Private Link/WAF) with explicit cost approval. DNS weighting is failover-experiment only, never the canary mechanism.

## Why

Standard has the required capabilities (weighted origin groups, external/AWS origins, health probes, single hostname) without Premium cost. DNS distributes responses not requests and is TTL-nondeterministic — unsuitable for 1%/5% gates with minimum-volume statistics.

## Trade-offs

Adds an Azure dependency to the traffic plane (even AWS-origin traffic traverses it during migration). Global resource needs explicit cost review + ephemeral lifetime + cleanup verification. Observed split ≠ desired weight at low volume — gates use observed telemetry.

## Consequences

Canonical stages 0/1/5/25/50/100 (read-only until Stage 5 write transfer); per-stage error/latency/health/DB/queue/lag/drift gates; desired + observed recorded; post-write rollback blocked without reverse sync.

## Evidence

Pending implementation validation (verify external-origin + weighted routing against current Azure docs before first apply; record links + test refs in compatibility report).
