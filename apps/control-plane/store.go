// Store abstraction: MemStore default; PGStore when DATABASE_URL is set.
package main

// AuditEntry mirrors audit_events (DOMAIN_MODEL.md AuditEvent).
// JSON tags serve the read-only audit timeline endpoint; no existing
// serialized shape changes (nothing else marshals AuditEntry today).
type AuditEntry struct {
	RunID               string `json:"run_id"`
	WorkloadID          string `json:"workload_id"`
	ActorType           string `json:"actor_type"`
	ActorID             string `json:"actor_id"`
	Action              string `json:"action"`
	Resource            string `json:"resource"`
	RequestID           string `json:"request_id"`
	IdempotencyKey      string `json:"idempotency_key"`
	PolicyBundleVersion string `json:"policy_bundle_version"`
	PolicyDecision      string `json:"policy_decision"`
	ApprovalID          string `json:"approval_id"`
	Result              string `json:"result"` // success|failure|denied
	Metadata            string `json:"metadata"` // JSON
	CreatedAt           string `json:"created_at"` // RFC3339; set by the store on record
}

// Store is the control-plane persistence boundary.
//
// Idempotency contract (see docs/IDEMPOTENCY.md): CheckIdem returns the stored
// SHA-256 request_hash alongside the response. An empty hash means a legacy
// pre-migration row, which replays without conflict comparison. SaveIdem must
// persist the hash; retention is 24h (expired records behave as absent).
type Store interface {
	CreateWorkload(id string, wl map[string]any) error
	GetWorkload(id string) (map[string]any, bool)
	ListWorkloads() []map[string]any
	CreateMigration(id string, run map[string]any) error
	GetMigration(id string) (map[string]any, bool)
	// ListMigrations lists migration runs for one workload (read-only
	// console discovery; chronological by insertion in MemStore).
	ListMigrations(workloadID string) []map[string]any
	CheckIdem(key string) (resp []byte, status int, requestHash string, found bool)
	SaveIdem(key, requestHash string, resp []byte, status int)
	SaveCompatReport(rep CompatReport) error
	GetLatestCompatReport(workloadID string) (CompatReport, bool)
	SaveTargetPlan(plan TargetMigrationPlan) error
	GetLatestTargetPlan(workloadID string) (TargetMigrationPlan, bool)
	SaveDriftReport(rep DriftReport) error
	GetDriftReports(workloadID, status string) []DriftReport
	GetLatestDriftReport(workloadID, planID string) (DriftReport, bool)
	SaveApproval(appr Approval) error
	GetApproval(id string) (Approval, bool)
	GetApprovals(workloadID, status string) []Approval
	// DecideApproval compare-and-sets pending -> decided; reports whether
	// this call performed the transition (false = already settled).
	DecideApproval(appr Approval) (bool, error)
	SaveExecution(ex Execution) error
	GetExecution(workflowID string) (Execution, bool)
	GetExecutionsForMigration(migrationID string) []Execution
	UpdateExecutionStatus(workflowID, status, errMsg string) error
	// Canary stage history (ordered evaluations per migration).
	SaveCanaryRecord(rec CanaryRecord) error
	GetCanaryRecords(migrationID string) []CanaryRecord
	// Write-ownership record. Absent means AWS authoritative (v1 initial).
	// TransferOwnership atomically moves expectOwner -> rec.CurrentOwner;
	// false means the current owner differs (exactly-once transfer).
	GetOwnership(migrationID string) (OwnershipRecord, bool)
	TransferOwnership(migrationID, expectOwner string, rec OwnershipRecord) (bool, error)
	HasAuditRequest(requestID string) bool
	RecordAudit(e AuditEntry) error
	// GetAuditForMigration lists audit entries scoped to one migration
	// (RunID == migrationID) in chronological order. Read-only timeline
	// source for the console; never an authorization input.
	GetAuditForMigration(migrationID string) []AuditEntry
	Ping() error
	Close() error
}
