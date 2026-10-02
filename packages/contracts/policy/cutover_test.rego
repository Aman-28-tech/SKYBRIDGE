package skybridge.cutover

import rego.v1

base_input := {
	"action": "shift_traffic",
	"environment": "dev",
	"target_weight": 1,
	"canary_stage": 1,
	"read_only_canary": true,
	"write_ownership": "aws",
	"reverse_sync_ready": false,
	"validation_status": "passed",
	"compatibility_status": "pass",
	"blocking_drift_count": 0,
	"security_critical_drift_count": 0,
	"target_healthy": true,
	"cdc_lag_seconds": 8,
	"rpo_seconds": 30,
}

test_dev_stage1_allowed if {
	allow with input as base_input
}

test_dev_stage5_allowed if {
	allow with input as object.union(base_input, {"target_weight": 5})
}

test_lag_blocks_cutover if {
	not allow with input as object.union(base_input, {"cdc_lag_seconds": 31})
}

test_validation_blocks_cutover if {
	not allow with input as object.union(base_input, {"validation_status": "failed"})
}

test_conditional_compat_blocks_auto if {
	not allow with input as object.union(base_input, {"compatibility_status": "conditional"})
}

test_unknown_compat_blocks_auto if {
	not allow with input as object.union(base_input, {"compatibility_status": "unknown"})
}

test_block_compat_blocks_auto if {
	not allow with input as object.union(base_input, {"compatibility_status": "block"})
}

test_blocking_drift_blocks if {
	not allow with input as object.union(base_input, {"blocking_drift_count": 1})
}

test_security_critical_drift_blocks if {
	not allow with input as object.union(base_input, {"security_critical_drift_count": 1})
}

test_staging_large_cutover_requires_denial_in_v1 if {
	not allow with input as object.union(base_input, {
		"environment": "staging",
		"target_weight": 50,
	})
}

test_staging_25_allowed if {
	allow with input as object.union(base_input, {
		"environment": "staging",
		"target_weight": 25,
	})
}

test_production_like_never_auto_allowed if {
	not allow with input as object.union(base_input, {"environment": "production-like"})
}

test_production_like_requires_approval if {
	requires_approval with input as object.union(base_input, {"environment": "production-like"})
}

test_non_canonical_weight_denied if {
	not allow with input as object.union(base_input, {"target_weight": 10})
	"non_canonical_weight" in deny_reason with input as object.union(base_input, {"target_weight": 10})
}

test_write_canary_violation_denied if {
	not allow with input as object.union(base_input, {"read_only_canary": false})
}

test_post_write_rollback_blocked if {
	"post_write_rollback_blocked" in deny_reason with input as {
		"action": "rollback_traffic",
		"environment": "dev",
		"target_weight": 0,
		"write_ownership": "azure",
		"reverse_sync_ready": false,
		"validation_status": "passed",
		"compatibility_status": "pass",
		"blocking_drift_count": 0,
		"security_critical_drift_count": 0,
		"target_healthy": true,
		"cdc_lag_seconds": 5,
		"rpo_seconds": 30,
	}
}

test_malformed_input_denied if {
	not allow with input as {"action": "shift_traffic"}
}
