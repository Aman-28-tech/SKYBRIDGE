# ADR — Capability Registry YAML Parsing (control-plane: gopkg.in/yaml.v3)

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-25

Per `docs/TECH_STACK.md` deferred-technology rule.

## Problem

The compatibility evaluator and target planner must read the repository's capability registry, whose canonical format is YAML (`capability-registry.example.yaml`). Go's standard library has no YAML parser.

## Options

- `gopkg.in/yaml.v3 v3.0.1` (ecosystem standard, stable, no cgo)
- Convert the registry to JSON (invents a second canonical format; rejected)
- Hand-written YAML subset parser (fragile; rejected)

## Decision

`gopkg.in/yaml.v3 v3.0.1`, pinned in `go.mod`/`go.sum`, used only to parse the registry (stdlib `encoding/json` remains the API wire format).

## Why

Smallest change consistent with the existing specification: the registry stays single-source YAML, parsed by the standard library for the job.

## Trade-offs / operational cost

One pinned dependency with no transitive requirements. Registry embedded byte-identical (`apps/control-plane/registry/`) with a sync test, mirroring the schema-copy pattern.

## Test plan

`compat_test.go`: registry load (9 capabilities, version 1), sync test, full A–M matrix. `go test -race ./...` green.

## Evidence

Implementation validation: compatibility slice test run.
