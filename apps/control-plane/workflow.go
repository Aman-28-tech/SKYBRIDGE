// Temporal workflow definitions land here in Phase 8 (Temporal Go SDK).
// This baseline file owns the pure state-transition table mirroring
// docs/WORKFLOW_STATE_MACHINE.md — no SDK dependency, no side effects.
//
// Valid edges (forward): REGISTERED->DISCOVERED->MODELED->COMPATIBLE->PLANNED->
// APPROVED->PROVISIONING->TARGET_READY->REPLICATING->REHEARSING->VALIDATING->
// READY_FOR_CUTOVER->CUTTING_OVER->VERIFYING->COMPLETED
// Lateral: any non-terminal -> RETRYING|PAUSED|FAILED; FAILED -> RECOVERY|ROLLING_BACK|ABORTED;
// ROLLING_BACK -> ROLLED_BACK; terminal: COMPLETED|ROLLED_BACK|ABORTED.
// Rollback rule: post-write traffic-only rollback denied without reverse sync.
package main

// ValidTransitions mirrors the state machine. Temporal activities check
// CanTransition before persisting any step completion.
var ValidTransitions = map[string][]string{
	"REGISTERED":       {"DISCOVERED", "RETRYING", "PAUSED", "FAILED"},
	"DISCOVERED":       {"MODELED", "RETRYING", "PAUSED", "FAILED"},
	"MODELED":          {"COMPATIBLE", "RETRYING", "PAUSED", "FAILED"},
	"COMPATIBLE":       {"PLANNED", "RETRYING", "PAUSED", "FAILED"},
	"PLANNED":          {"APPROVED", "RETRYING", "PAUSED", "FAILED"},
	"APPROVED":         {"PROVISIONING", "RETRYING", "PAUSED", "FAILED"},
	"PROVISIONING":     {"TARGET_READY", "RETRYING", "PAUSED", "FAILED"},
	"TARGET_READY":     {"REPLICATING", "RETRYING", "PAUSED", "FAILED"},
	"REPLICATING":      {"REHEARSING", "RETRYING", "PAUSED", "FAILED"},
	"REHEARSING":       {"VALIDATING", "RETRYING", "PAUSED", "FAILED"},
	"VALIDATING":       {"READY_FOR_CUTOVER", "RETRYING", "PAUSED", "FAILED"},
	"READY_FOR_CUTOVER": {"CUTTING_OVER", "RETRYING", "PAUSED", "FAILED"},
	"CUTTING_OVER":     {"VERIFYING", "ROLLING_BACK", "RETRYING", "PAUSED", "FAILED"},
	"VERIFYING":        {"COMPLETED", "ROLLING_BACK", "RETRYING", "PAUSED", "FAILED"},
	"RETRYING":         {"DISCOVERED", "MODELED", "PROVISIONING", "REPLICATING", "FAILED", "PAUSED"},
	"PAUSED":           {"RETRYING", "FAILED", "ABORTED"},
	"FAILED":           {"RECOVERY", "ROLLING_BACK", "ABORTED"},
	"RECOVERY":         {"RETRYING", "ROLLING_BACK", "ABORTED"},
	"ROLLING_BACK":     {"ROLLED_BACK", "FAILED"},
	"COMPLETED":        {},
	"ROLLED_BACK":      {},
	"ABORTED":          {},
}

// CanTransition reports whether from->to is legal.
func CanTransition(from, to string) bool {
	for _, n := range ValidTransitions[from] {
		if n == to {
			return true
		}
	}
	return false
}

// PostWriteRollbackBlocked enforces the v1 data-safety rule.
func PostWriteRollbackBlocked(writeOwnership string, reverseSyncReady bool) bool {
	return writeOwnership == "azure" && !reverseSyncReady
}
