// MemStore: in-process control-plane store (default).
package main

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// idemTTL is the v1 retention period: reuse after expiry is a new request.
const idemTTL = 24 * time.Hour

type MemStore struct {
	mu         sync.RWMutex
	workloads  map[string]map[string]any
	migrations map[string]map[string]any
	idem       map[string]idemResp
	compat       map[string]CompatReport
	compatLatest map[string]string // workloadID -> report ID
	plans        map[string]TargetMigrationPlan
	planLatest   map[string]string // workloadID -> plan ID
	drifts       map[string]DriftReport
	driftOrder   []string // insertion order for latest-for-plan semantics
	approvals    map[string]Approval
	executions   map[string]Execution
	canary       map[string][]CanaryRecord // migrationID -> ordered history
	ownership    map[string]OwnershipRecord
	audits     []AuditEntry
}

type idemResp struct {
	body        []byte
	status      int
	requestHash string
	expiresAt   time.Time
}

func NewMemStore() *MemStore {
	return &MemStore{
		workloads:  map[string]map[string]any{},
		migrations: map[string]map[string]any{},
		idem:       map[string]idemResp{},
		compat:     map[string]CompatReport{},
		compatLatest: map[string]string{},
		plans:      map[string]TargetMigrationPlan{},
		planLatest: map[string]string{},
		drifts:     map[string]DriftReport{},
		approvals:  map[string]Approval{},
		executions: map[string]Execution{},
		canary:     map[string][]CanaryRecord{},
		ownership:  map[string]OwnershipRecord{},
	}
}

func (m *MemStore) CreateWorkload(id string, wl map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workloads[id] = wl
	return nil
}

func (m *MemStore) GetWorkload(id string) (map[string]any, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	wl, ok := m.workloads[id]
	return wl, ok
}

func (m *MemStore) ListWorkloads() []map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]map[string]any, 0, len(m.workloads))
	for _, v := range m.workloads {
		items = append(items, v)
	}
	return items
}

func (m *MemStore) CreateMigration(id string, run map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.migrations[id] = run
	return nil
}

func (m *MemStore) GetMigration(id string) (map[string]any, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	run, ok := m.migrations[id]
	return run, ok
}

// ListMigrations lists migration runs scoped to one workload.
func (m *MemStore) ListMigrations(workloadID string) []map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []map[string]any
	for _, run := range m.migrations {
		if wid, _ := run["workload_id"].(string); wid == workloadID {
			out = append(out, run)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		idi, _ := out[i]["id"].(string)
		idj, _ := out[j]["id"].(string)
		return idi < idj
	})
	return out
}

func (m *MemStore) CheckIdem(key string) ([]byte, int, string, bool) {
	m.mu.RLock()
	r, ok := m.idem[key]
	m.mu.RUnlock()
	if !ok {
		return nil, 0, "", false
	}
	if time.Now().After(r.expiresAt) {
		m.mu.Lock()
		delete(m.idem, key)
		m.mu.Unlock()
		return nil, 0, "", false
	}
	return r.body, r.status, r.requestHash, true
}

func (m *MemStore) SaveIdem(key, requestHash string, resp []byte, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.idem[key] = idemResp{body: resp, status: status, requestHash: requestHash, expiresAt: time.Now().Add(idemTTL)}
}

func (m *MemStore) SaveCompatReport(rep CompatReport) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.compat[rep.ID] = rep
	m.compatLatest[rep.WorkloadID] = rep.ID
	return nil
}

func (m *MemStore) GetLatestCompatReport(workloadID string) (CompatReport, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.compatLatest[workloadID]
	if !ok {
		return CompatReport{}, false
	}
	rep, ok := m.compat[id]
	return rep, ok
}

func (m *MemStore) SaveTargetPlan(plan TargetMigrationPlan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plans[plan.ID] = plan
	m.planLatest[plan.WorkloadID] = plan.ID
	return nil
}

func (m *MemStore) GetLatestTargetPlan(workloadID string) (TargetMigrationPlan, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.planLatest[workloadID]
	if !ok {
		return TargetMigrationPlan{}, false
	}
	plan, ok := m.plans[id]
	return plan, ok
}

func (m *MemStore) SaveDriftReport(rep DriftReport) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.drifts[rep.ID]; exists {
		// Re-observation: preserve the existing report object, refresh
		// recency only by moving the ID to the latest position (no duplicate).
		for i, id := range m.driftOrder {
			if id == rep.ID {
				m.driftOrder = append(m.driftOrder[:i:i], m.driftOrder[i+1:]...)
				break
			}
		}
		m.driftOrder = append(m.driftOrder, rep.ID)
		return nil
	}
	m.drifts[rep.ID] = rep
	m.driftOrder = append(m.driftOrder, rep.ID)
	return nil
}

func (m *MemStore) GetDriftReports(workloadID, status string) []DriftReport {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []DriftReport
	for _, r := range m.drifts {
		if r.WorkloadID != workloadID {
			continue
		}
		if status != "" && r.Status != status {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

func (m *MemStore) GetLatestDriftReport(workloadID, planID string) (DriftReport, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := len(m.driftOrder) - 1; i >= 0; i-- {
		if rep, ok := m.drifts[m.driftOrder[i]]; ok &&
			rep.WorkloadID == workloadID && rep.DesiredReference == planID {
			return rep, true
		}
	}
	return DriftReport{}, false
}

func (m *MemStore) SaveApproval(appr Approval) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.approvals[appr.ID] = appr
	return nil
}

func (m *MemStore) GetApproval(id string) (Approval, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	appr, ok := m.approvals[id]
	return appr, ok
}

func (m *MemStore) GetApprovals(workloadID, status string) []Approval {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Approval
	for _, a := range m.approvals {
		if a.WorkloadID != workloadID {
			continue
		}
		if status != "" && a.Decision != status {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (m *MemStore) DecideApproval(appr Approval) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.approvals[appr.ID]
	if !ok || cur.Decision != ApprovalPending {
		return false, nil
	}
	m.approvals[appr.ID] = appr
	return true, nil
}

func (m *MemStore) SaveExecution(ex Execution) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.executions[ex.WorkflowID] = ex
	return nil
}

func (m *MemStore) GetExecution(workflowID string) (Execution, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ex, ok := m.executions[workflowID]
	return ex, ok
}

func (m *MemStore) GetExecutionsForMigration(migrationID string) []Execution {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Execution
	for _, ex := range m.executions {
		if ex.MigrationID == migrationID {
			out = append(out, ex)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WorkflowID < out[j].ID })
	return out
}

func (m *MemStore) UpdateExecutionStatus(workflowID, status, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ex, ok := m.executions[workflowID]
	if !ok {
		return fmt.Errorf("execution not found")
	}
	ex.Status = status
	ex.Error = errMsg
	m.executions[workflowID] = ex
	return nil
}

func (m *MemStore) SaveCanaryRecord(rec CanaryRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.canary[rec.MigrationID] = append(m.canary[rec.MigrationID], rec)
	return nil
}

func (m *MemStore) GetCanaryRecords(migrationID string) []CanaryRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rows := m.canary[migrationID]
	out := make([]CanaryRecord, len(rows))
	copy(out, rows)
	return out
}

// OwnershipRecord is the single authoritative-owner fact for a migration.
// Absent = AWS authoritative. Only aws->azure is permitted in v1.
type OwnershipRecord struct {
	MigrationID     string `json:"migration_id"`
	WorkloadID      string `json:"workload_id"`
	CurrentOwner    string `json:"current_owner"`
	PreviousOwner   string `json:"previous_owner"`
	Routing         string `json:"routing"`
	PolicyInputHash string `json:"policy_input_hash"`
	ApprovalID      string `json:"approval_id"`
	SourceLSN       string `json:"source_lsn"`
	AppliedLSN      string `json:"applied_lsn"`
	LagSeconds      int64  `json:"cdc_lag_seconds"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

func (m *MemStore) GetOwnership(migrationID string) (OwnershipRecord, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.ownership[migrationID]
	return rec, ok
}

// TransferOwnership allows exactly one transition: (absent|aws) -> azure.
// Reversal to aws and re-commit over azure are both refused, so the store
// can never express dual authority or an ownership ping-pong.
func (m *MemStore) TransferOwnership(migrationID, expectOwner string, rec OwnershipRecord) (bool, error) {
	if rec.CurrentOwner != "azure" {
		return false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.ownership[migrationID]; ok {
		if cur.CurrentOwner != "aws" || expectOwner != "aws" {
			return false, nil
		}
	} else if expectOwner != "aws" {
		return false, nil
	}
	m.ownership[migrationID] = rec
	return true, nil
}

func (m *MemStore) HasAuditRequest(requestID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.audits {
		if a.RequestID == requestID {
			return true
		}
	}
	return false
}

func (m *MemStore) RecordAudit(e AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.CreatedAt == "" {
		e.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	m.audits = append(m.audits, e)
	return nil
}

// GetAuditForMigration returns migration-scoped entries (RunID match) in
// insertion (chronological) order.
func (m *MemStore) GetAuditForMigration(migrationID string) []AuditEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []AuditEntry{}
	for _, a := range m.audits {
		if a.RunID == migrationID {
			out = append(out, a)
		}
	}
	return out
}

func (m *MemStore) AuditCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.audits)
}

func (m *MemStore) Ping() error { return nil }
func (m *MemStore) Close() error { return nil }
