// Lag and dedupe primitives for the replication applier.
// cdc_lag_seconds = target_apply_observation_time - source_commit_timestamp.
// Clocks must be NTP-synced (±1s) for the 30s RPO gate to be meaningful.
//
// Reuses the ReplicationStatus concepts from packages/contracts/proto/
// migration.proto (source_lsn/target_lsn/cdc_lag_seconds/applied_events/
// source_commit_unix/apply_observation_unix); no competing model is invented.
//
// RPO target: 30 seconds (WithinRPO). RPO is met only when measured from
// source/applied evidence (see LagSample), never asserted without data.
package cdc

// RPOTargetSeconds is the CloudShop design target (not a claim).
const RPOTargetSeconds int64 = 30

// LagSeconds returns applyUnix - commitUnix (may be negative on clock skew;
// callers treat negative as 0 + record a skew warning).
func LagSeconds(commitUnix, applyUnix int64) int64 {
	return applyUnix - commitUnix
}

// WithinRPO reports whether observed lag satisfies the requirement.
func WithinRPO(commitUnix, applyUnix, rpoSeconds int64) bool {
	lag := LagSeconds(commitUnix, applyUnix)
	if lag < 0 {
		lag = 0
	}
	return lag <= rpoSeconds
}

// AppliedSet is the legacy in-memory duplicate guard kept for the original
// unit test. Durable per-event dedupe is cdc_applied_events (see apply.go);
// AppliedSet must not be used as the application skip check because a shared
// txn_id across E1/E2/E3 would corrupt E2/E3 (see event.go SEMANTICS).
type AppliedSet struct {
	seen map[string]bool
}

func NewAppliedSet() *AppliedSet { return &AppliedSet{seen: map[string]bool{}} }

// Apply records txnID; returns true if newly applied, false if duplicate.
func (a *AppliedSet) Apply(txnID string) bool {
	if a.seen[txnID] {
		return false
	}
	a.seen[txnID] = true
	return true
}

// LagSample is the measurable source/applied evidence for one observation.
// Mirrors ReplicationStatus: source_lsn/target_lsn + commit/observation times.
type LagSample struct {
	SourceLSN       string
	AppliedLSN      string
	SourceCommitUnix int64
	ObserveUnix      int64
	AppliedEvents    uint64
}

// Lag reports cdc_lag_seconds for the sample (clamped >= 0 on clock skew).
func (s LagSample) Lag() int64 {
	lag := LagSeconds(s.SourceCommitUnix, s.ObserveUnix)
	if lag < 0 {
		lag = 0
	}
	return lag
}

// MeetsRPO reports Lag() <= 30. False without evidence is a breach, never a pass.
func (s LagSample) MeetsRPO() bool {
	return s.Lag() <= RPOTargetSeconds
}
