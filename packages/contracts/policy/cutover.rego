package skybridge.cutover

import rego.v1

# Canonical compatibility: pass|conditional|unknown|block (only pass auto-allows).
# Canonical drift severities: informational|blocking|security_critical.
# Canonical stages: 0|1|5|25|50|100. Stages 1-4 are read-only canary.
# cdc_lag_seconds = target_apply_observation_time - source_commit_timestamp.

default allow := false
default requires_approval := true

canonical_stage contains w if {
	some w in [0, 1, 5, 25, 50, 100]
	w == input.target_weight
}

base_gate if {
	input.action == "shift_traffic"
	input.validation_status == "passed"
	input.compatibility_status == "pass"
	input.blocking_drift_count == 0
	object.get(input, "security_critical_drift_count", 0) == 0
	input.cdc_lag_seconds <= input.rpo_seconds
	input.target_healthy == true
	canonical_stage[input.target_weight]
}

# Dev: any canonical stage allowed when base gate holds and Azure not yet writer
# (or 100% only via explicit ownership-transfer flow flag).
allow if {
	base_gate
	input.environment == "dev"
	input.write_ownership == "aws"
	input.read_only_canary == true
	input.target_weight in [0, 1, 5, 25, 50]
}

allow if {
	base_gate
	input.environment == "dev"
	input.write_ownership == "aws"
	input.ownership_transfer_verified == true
	input.target_weight == 100
}

# Staging: cap at 25% without approval; higher stages need approval path (not allow).
allow if {
	base_gate
	input.environment == "staging"
	input.write_ownership == "aws"
	input.read_only_canary == true
	input.target_weight in [0, 1, 5, 25]
}

# Local/experiment: same as dev (read-only canary only until transfer verified).
allow if {
	base_gate
	input.environment in ["local", "experiment"]
	input.write_ownership == "aws"
	input.read_only_canary == true
	input.target_weight in [0, 1, 5, 25, 50]
}

# Production-like is never auto-allowed; it always requires approval.
# (No allow rule for production-like.)

requires_approval if {
	input.environment == "production-like"
}

requires_approval if {
	input.environment == "staging"
	input.target_weight > 25
}

requires_approval if {
	input.compatibility_status == "conditional"
}

# Post-write traffic-only rollback is blocked unless reverse sync is ready.
deny_reason contains "post_write_rollback_blocked" if {
	input.action == "rollback_traffic"
	input.write_ownership == "azure"
	not input.reverse_sync_ready == true
}

deny_reason contains "cdc_lag_exceeds_rpo" if {
	input.cdc_lag_seconds > input.rpo_seconds
}

deny_reason contains "validation_failed" if {
	input.validation_status != "passed"
}

deny_reason contains "compatibility_not_pass" if {
	not input.compatibility_status == "pass"
}

deny_reason contains "blocking_drift" if {
	input.blocking_drift_count > 0
}

deny_reason contains "security_critical_drift" if {
	object.get(input, "security_critical_drift_count", 0) > 0
}

deny_reason contains "target_unhealthy" if {
	not input.target_healthy == true
}

deny_reason contains "non_canonical_weight" if {
	not canonical_stage[input.target_weight]
}

deny_reason contains "write_ownership_violation" if {
	input.action == "shift_traffic"
	input.target_weight != 100
	input.write_ownership != "aws"
}

deny_reason contains "read_only_canary_violation" if {
	input.action == "shift_traffic"
	input.target_weight in [1, 5, 25, 50]
	not input.read_only_canary == true
}
