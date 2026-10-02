# ADR — Kafka vs RabbitMQ for CDC (v1: Kafka-compatible log)

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 1
> Last updated: 2026-09-25

## Context

Postgres CDC (Debezium) needs a durable, replayable event log with offsets; CloudShop app queue needs at-least-once task delivery. Candidates: Kafka(-compatible), RabbitMQ, cloud-native queues.

## Options

- Kafka-compatible durable log (offsets, replay, partitioning)
- RabbitMQ (routing flexibility, weaker replay/offset story)
- Cloud queues only (SQS/Service Bus; provider-locked, no local story)

## Decision

Kafka-compatible durable event log for Debezium CDC (local: Redpanda; cloud: managed Kafka-surface validated per milestone). RabbitMQ remains suitable for unrelated task workloads but is not the CDC transport. App queue semantics stay at-least-once + duplicate-safe regardless of broker.

## Why

CDC needs offsets + replay + durable retention + consumer-group resume — Kafka's model. Redpanda gives a single-binary local equivalent without ZooKeeper.

## Trade-offs

Higher operational complexity than a simple queue (partitions, retention, schema, lag monitoring). Must operate broker + connectors + applier + offset/applied-txn stores.

## Consequences

Pipeline: Debezium → Kafka-compatible log → applier with durable offset + applied-txn marker; `cdc_lag_seconds` tracked separately from LSN; chaos covers duplicate/restart/lag/DB-failure.

## Evidence

Pending implementation validation (Phase 7 duplicate/restart/lag tests).
