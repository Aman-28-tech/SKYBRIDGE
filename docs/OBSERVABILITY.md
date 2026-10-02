# Observability

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 2
> Last updated: 2026-09-25

## Three signals

- metrics
- logs
- traces

## Correlation

Every operation carries:

```text
request_id
run_id
workload_id
trace_id
```

## Migration metrics

```text
migration_runs_total
migration_failures_total
migration_duration_seconds (start trigger -> COMPLETED/ROLLED_BACK)
migration_step_duration_seconds (per step_id)
migration_step_retries_total
migration_rollbacks_total (pre_write vs post_write_blocked labels)
```

## Replication metrics

```text
cdc_lag_seconds (canonical freshness gauge)
replication_lag_seconds (alias; prefer cdc_lag_seconds)
replication_events_total
replication_errors_total
replication_consumer_restarts_total
source_lsn / target_applied_lsn (gauges)
queue_depth (gauge at final drain)
```

## Cutover metrics

```text
cutover_stage_changes_total
cutover_stage_duration_seconds
observed_source_traffic_ratio
observed_target_traffic_ratio
cutover_rollbacks_total
```

## AI metrics

```text
agent_runs_total
agent_latency_seconds
tool_calls_total
tool_failures_total
policy_denials_total
approval_requests_total
plan_validation_failures_total
```

## Design targets

These are engineering targets, not measured claims:

- RPO gate: `cdc_lag_seconds <= 30s` at final sync (CloudShop requirement; see STATE_REPLICATION.md)
- policy p95: < 100 ms locally
- control-plane API p95: < 500 ms under defined baseline load: `100 req/s sustained 15 min, 50% reads / 50% order creates, local kind cluster, PG+Redis local`

## SLI/SLO (v1 design targets)

| Signal | SLI | SLO target |
|---|---|---|
| Control-plane availability | successful responses / total (5xx-excluded client errors) | 99.5% over 30d dev |
| API latency | p95/p99 per endpoint | p95 < 500ms, p99 < 1s at baseline load |
| Workflow durability | runs resumed after worker restart / restarts | 100% |
| Replication freshness | `cdc_lag_seconds` | <= 30s at final sync gate |
| Audit completeness | mutations with audit intent+completion / mutations | 100% |

## Alerts

Initial alert classes:

### Critical

- unauthorized mutation attempt
- policy engine unavailable during mutation
- replication lag above RPO
- cutover error-rate gate breached
- rollback workflow failure

### Warning

- drift detected
- CDC lag increasing
- queue depth increasing
- cloud API throttling

## Tracing

Trace:

```text
migration
 -> workflow
 -> activity
 -> provider call
 -> verification
```

AI trace:

```text
agent run
 -> prompt/version
 -> tool call
 -> tool result
 -> policy result
 -> final proposal
```

## Sampling

For migration workflows:

```text
100% trace retention for the run
```

For ordinary high-volume workload traffic:

```text
sampled traces
```

The sampling policy must not remove evidence needed for incident reconstruction.

## Retention (decided, not a range)

- local telemetry: 7 days
- cloud dev/staging telemetry: 30 days (reduce to 14 only with cost-review note in experiment record)

Actual cloud retention is configured per environment in Terraform and recorded in the experiment record.

## Sensitive data

Never log:

- credentials
- access tokens
- private keys
- full database secrets
- unnecessary user PII

## Dashboards

Minimum dashboards:

1. SKYBRIDGE control-plane health
2. migration runs
3. replication
4. cutover
5. policy/security
6. agent/tool execution
