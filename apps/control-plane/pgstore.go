// PGStore: Postgres control-plane store (used when DATABASE_URL is set).
// Schema: apps/control-plane/migrations/001_init.sql.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "github.com/lib/pq"
)

type PGStore struct {
	db *sql.DB
}

func NewPGStore(url string) (*PGStore, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pg ping: %w", err)
	}
	// Additive hardening schema (idempotent; safe on pre-existing volumes):
	// workload owner for object-level authorization (H-2).
	if _, err := db.Exec(`ALTER TABLE workloads ADD COLUMN IF NOT EXISTS owner_id TEXT`); err != nil {
		db.Close()
		return nil, fmt.Errorf("pg ensure owner_id: %w", err)
	}
	return &PGStore{db: db}, nil
}

func (p *PGStore) CreateWorkload(id string, wl map[string]any) error {
	name, _ := wl["name"].(string)
	owner, _ := wl["owner_id"].(string)
	sv, _ := wl["schema_version"].(int)
	if sv == 0 {
		if f, ok := wl["schema_version"].(float64); ok {
			sv = int(f)
		} else {
			sv = 1
		}
	}
	state, _ := wl["lifecycle_state"].(string)
	specJSON := []byte("{}")
	if cs, ok := wl["canonical_spec"]; ok {
		if b, err := json.Marshal(cs); err == nil {
			specJSON = b
		}
	}
	_, err := p.db.Exec(`INSERT INTO workloads (id, name, schema_version, lifecycle_state, canonical_spec, owner_id)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6,'')) ON CONFLICT (id) DO NOTHING`, id, name, sv, state, string(specJSON), owner)
	return err
}

func (p *PGStore) GetWorkload(id string) (map[string]any, bool) {
	var name, state, specRaw string
	var sv int
	var owner sql.NullString
	err := p.db.QueryRow(`SELECT name, schema_version, lifecycle_state, canonical_spec::text, owner_id FROM workloads WHERE id = $1`, id).
		Scan(&name, &sv, &state, &specRaw, &owner)
	if err != nil {
		return nil, false
	}
	var spec any
	if err := json.Unmarshal([]byte(specRaw), &spec); err != nil {
		spec = map[string]any{}
	}
	return map[string]any{"id": id, "name": name, "schema_version": sv,
		"lifecycle_state": state, "canonical_spec": spec, "owner_id": owner.String}, true
}

func (p *PGStore) ListWorkloads() []map[string]any {
	rows, err := p.db.Query(`SELECT id::text, name, schema_version, lifecycle_state, canonical_spec::text, owner_id FROM workloads ORDER BY created_at`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var items []map[string]any
	for rows.Next() {
		var id, name, state, specRaw string
		var sv int
		var owner sql.NullString
		if err := rows.Scan(&id, &name, &sv, &state, &specRaw, &owner); err != nil {
			continue
		}
		var spec any
		if err := json.Unmarshal([]byte(specRaw), &spec); err != nil {
			spec = map[string]any{}
		}
		items = append(items, map[string]any{"id": id, "name": name, "schema_version": sv,
			"lifecycle_state": state, "canonical_spec": spec, "owner_id": owner.String})
	}
	return items
}

func (p *PGStore) CreateMigration(id string, run map[string]any) error {
	wid, _ := run["workload_id"].(string)
	status, _ := run["status"].(string)
	step, _ := run["current_step"].(string)
	rid, _ := run["request_id"].(string)
	pbv, _ := run["policy_bundle_version"].(string)
	_, err := p.db.Exec(`INSERT INTO migrations (id, workload_id, status, current_step, request_id, policy_bundle_version)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (id) DO NOTHING`, id, wid, status, step, rid, pbv)
	return err
}

func (p *PGStore) GetMigration(id string) (map[string]any, bool) {
	var wid, status, step string
	err := p.db.QueryRow(`SELECT workload_id::text, status, current_step FROM migrations WHERE id = $1`, id).
		Scan(&wid, &status, &step)
	if err != nil {
		return nil, false
	}
	return map[string]any{"id": id, "workload_id": wid, "status": status, "current_step": step}, true
}

// ListMigrations lists migration runs scoped to one workload.
func (p *PGStore) ListMigrations(workloadID string) []map[string]any {
	rows, err := p.db.Query(`SELECT id::text, workload_id::text, status, current_step FROM migrations WHERE workload_id = $1 ORDER BY created_at`, workloadID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, wid, status, step string
		if err := rows.Scan(&id, &wid, &status, &step); err != nil {
			continue
		}
		out = append(out, map[string]any{"id": id, "workload_id": wid, "status": status, "current_step": step})
	}
	return out
}

func (p *PGStore) CheckIdem(key string) ([]byte, int, string, bool) {
	var resp []byte
	var status int
	var hash sql.NullString
	err := p.db.QueryRow(`SELECT response, status, request_hash FROM idempotency_records WHERE key = $1 AND expires_at > now()`, key).
		Scan(&resp, &status, &hash)
	if err != nil {
		return nil, 0, "", false
	}
	return resp, status, hash.String, true
}

func (p *PGStore) SaveIdem(key, requestHash string, resp []byte, status int) {
	_, _ = p.db.Exec(`INSERT INTO idempotency_records (key, response, status, request_hash) VALUES ($1, $2, $3, NULLIF($4,''))
		ON CONFLICT (key) DO NOTHING`, key, string(resp), status, requestHash)
}

func (p *PGStore) RecordAudit(e AuditEntry) error {	meta := e.Metadata
	if meta == "" {
		meta = "{}"
	}
	var metaJSON json.RawMessage = json.RawMessage(meta)
	if !json.Valid(metaJSON) {
		metaJSON = json.RawMessage(`{}`)
	}
	_, err := p.db.Exec(`INSERT INTO audit_events
		(id, run_id, workload_id, actor_type, actor_id, action, resource, request_id,
		 idempotency_key, policy_bundle_version, policy_decision, approval_id, result, metadata)
		VALUES (gen_random_uuid(), NULLIF($1,'')::uuid, NULLIF($2,'')::uuid, $3, $4, $5, $6, $7, $8, $9, NULLIF($10,''), NULLIF($11,'')::uuid, $12, $13)`,
		e.RunID, e.WorkloadID, e.ActorType, e.ActorID, e.Action, e.Resource, e.RequestID,
		e.IdempotencyKey, e.PolicyBundleVersion, e.PolicyDecision, e.ApprovalID, e.Result, string(metaJSON))
	return err
}

func (p *PGStore) SaveCompatReport(rep CompatReport) error {
	checks, err := json.Marshal(rep.Checks)
	if err != nil {
		return err
	}
	_, err = p.db.Exec(`INSERT INTO compatibility_reports
		(id, workload_id, target_provider, status, registry_version, evaluator_version, checks)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (id) DO NOTHING`,
		rep.ID, rep.WorkloadID, rep.TargetProvider, rep.Status,
		rep.RegistryVersion, rep.EvaluatorVersion, string(checks))
	return err
}

func (p *PGStore) GetLatestCompatReport(workloadID string) (CompatReport, bool) {
	var rep CompatReport
	var checksRaw string
	err := p.db.QueryRow(`SELECT id::text, workload_id::text, target_provider, status,
		registry_version, evaluator_version, checks::text
		FROM compatibility_reports WHERE workload_id = $1 ORDER BY created_at DESC LIMIT 1`, workloadID).
		Scan(&rep.ID, &rep.WorkloadID, &rep.TargetProvider, &rep.Status,
			&rep.RegistryVersion, &rep.EvaluatorVersion, &checksRaw)
	if err != nil {
		return CompatReport{}, false
	}
	if err := json.Unmarshal([]byte(checksRaw), &rep.Checks); err != nil {
		return CompatReport{}, false
	}
	return rep, true
}

func (p *PGStore) SaveTargetPlan(plan TargetMigrationPlan) error {
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	_, err = p.db.Exec(`INSERT INTO target_migration_plans
		(id, workload_id, compatibility_report_id, target_provider, overall_status,
		 registry_version, planner_version, plan)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET compatibility_report_id = EXCLUDED.compatibility_report_id,
			target_provider = EXCLUDED.target_provider, overall_status = EXCLUDED.overall_status,
			registry_version = EXCLUDED.registry_version, planner_version = EXCLUDED.planner_version,
			plan = EXCLUDED.plan`,
		plan.ID, plan.WorkloadID, plan.CompatibilityReportID, plan.TargetProvider,
		plan.OverallStatus, plan.CompatibilityRegistryVersion, plan.PlannerVersion, string(planJSON))
	return err
}

func (p *PGStore) GetLatestTargetPlan(workloadID string) (TargetMigrationPlan, bool) {
	var plan TargetMigrationPlan
	var planRaw string
	err := p.db.QueryRow(`SELECT id::text, workload_id::text, compatibility_report_id::text,
		target_provider, overall_status, registry_version, planner_version, plan::text
		FROM target_migration_plans WHERE workload_id = $1 ORDER BY created_at DESC LIMIT 1`, workloadID).
		Scan(&plan.ID, &plan.WorkloadID, &plan.CompatibilityReportID, &plan.TargetProvider,
			&plan.OverallStatus, &plan.CompatibilityRegistryVersion, &plan.PlannerVersion, &planRaw)
	if err != nil {
		return TargetMigrationPlan{}, false
	}
	if err := json.Unmarshal([]byte(planRaw), &plan); err != nil {
		return TargetMigrationPlan{}, false
	}
	return plan, true
}

func (p *PGStore) SaveDriftReport(rep DriftReport) error {
	diff, err := json.Marshal(map[string]any{
		"drift_gate": rep.DriftGate, "findings": rep.Findings,
		"planner_version": rep.PlanVersion, "registry_version": rep.RegistryVersion,
		"snapshot_version": rep.SnapshotVersion, "evaluator_version": rep.EvaluatorVersion,
	})
	if err != nil {
		return err
	}
	_, err = p.db.Exec(`INSERT INTO drift_reports
		(id, workload_id, severity, status, desired_reference, observed_reference, diff)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET detected_at = now()`,
		rep.ID, rep.WorkloadID, rep.Severity, rep.Status,
		rep.DesiredReference, rep.ObservedReference, string(diff))
	return err
}

func (p *PGStore) GetDriftReports(workloadID, status string) []DriftReport {
	query := `SELECT id::text, workload_id::text, severity, status,
		desired_reference, observed_reference, diff::text
		FROM drift_reports WHERE workload_id = $1`
	args := []any{workloadID}
	if status != "" {
		query += ` AND status = $2`
		args = append(args, status)
	}
	query += ` ORDER BY detected_at DESC`
	rows, err := p.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []DriftReport
	for rows.Next() {
		var rep DriftReport
		var diffRaw string
		if err := rows.Scan(&rep.ID, &rep.WorkloadID, &rep.Severity, &rep.Status,
			&rep.DesiredReference, &rep.ObservedReference, &diffRaw); err != nil {
			continue
		}
		var diff struct {
			DriftGate        string         `json:"drift_gate"`
			Findings         []DriftFinding `json:"findings"`
			PlannerVersion   string         `json:"planner_version"`
			RegistryVersion  int            `json:"registry_version"`
			SnapshotVersion  int            `json:"snapshot_version"`
			EvaluatorVersion string         `json:"evaluator_version"`
		}
		if err := json.Unmarshal([]byte(diffRaw), &diff); err != nil {
			continue
		}
		rep.DriftGate, rep.Findings = diff.DriftGate, diff.Findings
		rep.PlanVersion, rep.RegistryVersion = diff.PlannerVersion, diff.RegistryVersion
		rep.SnapshotVersion, rep.EvaluatorVersion = diff.SnapshotVersion, diff.EvaluatorVersion
		if rep.Findings == nil {
			rep.Findings = []DriftFinding{}
		}
		out = append(out, rep)
	}
	return out
}

func (p *PGStore) GetLatestDriftReport(workloadID, planID string) (DriftReport, bool) {
	var rep DriftReport
	var diffRaw string
	err := p.db.QueryRow(`SELECT id::text, workload_id::text, severity, status,
		desired_reference, observed_reference, diff::text
		FROM drift_reports WHERE workload_id = $1 AND desired_reference = $2
		ORDER BY detected_at DESC LIMIT 1`, workloadID, planID).
		Scan(&rep.ID, &rep.WorkloadID, &rep.Severity, &rep.Status,
			&rep.DesiredReference, &rep.ObservedReference, &diffRaw)
	if err != nil {
		return DriftReport{}, false
	}
	var diff struct {
		DriftGate        string         `json:"drift_gate"`
		Findings         []DriftFinding `json:"findings"`
		PlannerVersion   string         `json:"planner_version"`
		RegistryVersion  int            `json:"registry_version"`
		SnapshotVersion  int            `json:"snapshot_version"`
		EvaluatorVersion string         `json:"evaluator_version"`
	}
	if err := json.Unmarshal([]byte(diffRaw), &diff); err != nil {
		return DriftReport{}, false
	}
	rep.DriftGate, rep.Findings = diff.DriftGate, diff.Findings
	rep.PlanVersion, rep.RegistryVersion = diff.PlannerVersion, diff.RegistryVersion
	rep.SnapshotVersion, rep.EvaluatorVersion = diff.SnapshotVersion, diff.EvaluatorVersion
	if rep.Findings == nil {
		rep.Findings = []DriftFinding{}
	}
	return rep, true
}

func (p *PGStore) SaveApproval(appr Approval) error {
	_, err := p.db.Exec(`INSERT INTO approvals
		(id, workload_id, migration_id, action, risk, decision, requested_by, requested_by_type,
		 decided_by, decided_by_type, policy_bundle_version, policy_input_hash, target_provider,
		 plan_id, compatibility_report_id, drift_report_id, target_weight, environment, expires_at, decided_at)
		VALUES ($1, $2, NULLIF($3,'')::uuid, $4, $5, $6, $7, $8, NULLIF($9,''),
			NULLIF($10,''), $11, $12, $13,
			NULLIF($14,'')::uuid, NULLIF($15,'')::uuid, NULLIF($16,'')::uuid, $17, $18, $19, $20)
		ON CONFLICT (id) DO NOTHING`,
		appr.ID, appr.WorkloadID, appr.MigrationID, appr.Action, appr.Risk, appr.Decision,
		appr.RequestedBy, appr.RequestedByType, appr.DecidedBy, appr.DecidedByType,
		appr.PolicyBundleVersion, appr.PolicyInputHash, appr.TargetProvider,
		appr.PlanID, appr.CompatibilityReportID, appr.DriftReportID,
		appr.TargetWeight, appr.Environment, appr.ExpiresAt, nullableTime(appr.DecidedAt))
	return err
}

func nullableTime(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func scanApproval(row *sql.Row) (Approval, bool) {
	var a Approval
	var migrationID, decidedBy, decidedByType, decidedAt sql.NullString
	var planID, compatID, driftID sql.NullString
	err := row.Scan(&a.ID, &a.WorkloadID, &migrationID, &a.Action, &a.Risk, &a.Decision,
		&a.RequestedBy, &a.RequestedByType, &decidedBy, &decidedByType,
		&a.PolicyBundleVersion, &a.PolicyInputHash, &a.TargetProvider,
		&planID, &compatID, &driftID, &a.TargetWeight, &a.Environment,
		&a.ExpiresAt, &decidedAt)
	if err != nil {
		return Approval{}, false
	}
	a.MigrationID, a.DecidedBy, a.DecidedByType = migrationID.String, decidedBy.String, decidedByType.String
	a.PlanID, a.CompatibilityReportID, a.DriftReportID = planID.String, compatID.String, driftID.String
	a.DecidedAt = decidedAt.String
	return a, true
}

func (p *PGStore) GetApproval(id string) (Approval, bool) {
	return scanApproval(p.db.QueryRow(`SELECT id::text, workload_id::text,
		migration_id::text, action, risk, decision, requested_by, requested_by_type,
		decided_by, decided_by_type, policy_bundle_version, policy_input_hash, target_provider,
		plan_id::text, compatibility_report_id::text, drift_report_id::text,
		target_weight, environment, expires_at::text, decided_at::text
		FROM approvals WHERE id = $1`, id))
}

func (p *PGStore) GetApprovals(workloadID, status string) []Approval {
	query := `SELECT id::text, workload_id::text,
		migration_id::text, action, risk, decision, requested_by, requested_by_type,
		decided_by, decided_by_type, policy_bundle_version, policy_input_hash, target_provider,
		plan_id::text, compatibility_report_id::text, drift_report_id::text,
		target_weight, environment, expires_at::text, decided_at::text
		FROM approvals WHERE workload_id = $1`
	args := []any{workloadID}
	if status != "" {
		query += ` AND decision = $2`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := p.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	// Reuse scanApproval shape via row wrapper.
	var out []Approval
	for rows.Next() {
		var a Approval
		var migrationID, decidedBy, decidedByType, decidedAt sql.NullString
		var planID, compatID, driftID sql.NullString
		if err := rows.Scan(&a.ID, &a.WorkloadID, &migrationID, &a.Action, &a.Risk, &a.Decision,
			&a.RequestedBy, &a.RequestedByType, &decidedBy, &decidedByType,
			&a.PolicyBundleVersion, &a.PolicyInputHash, &a.TargetProvider,
			&planID, &compatID, &driftID, &a.TargetWeight, &a.Environment,
			&a.ExpiresAt, &decidedAt); err != nil {
			continue
		}
		a.MigrationID, a.DecidedBy, a.DecidedByType = migrationID.String, decidedBy.String, decidedByType.String
		a.PlanID, a.CompatibilityReportID, a.DriftReportID = planID.String, compatID.String, driftID.String
		a.DecidedAt = decidedAt.String
		out = append(out, a)
	}
	return out
}

func (p *PGStore) DecideApproval(appr Approval) (bool, error) {
	res, err := p.db.Exec(`UPDATE approvals SET decision = $2, decided_by = NULLIF($3,''),
		decided_by_type = NULLIF($4,''), decided_at = now()
		WHERE id = $1 AND decision = 'pending'`,
		appr.ID, appr.Decision, appr.DecidedBy, appr.DecidedByType)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return err == nil && n == 1, err
}

func (p *PGStore) SaveExecution(ex Execution) error {
	_, err := p.db.Exec(`INSERT INTO executions
		(id, workflow_id, run_id, migration_id, workload_id, target_weight, status,
		 policy_input_hash, approval_id, error)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, NULLIF($8,'')::uuid, $9)
		ON CONFLICT (workflow_id) DO NOTHING`,
		ex.WorkflowID, ex.RunID, ex.MigrationID, ex.WorkloadID, ex.TargetWeight,
		ex.Status, ex.PolicyInputHash, ex.ApprovalID, ex.Error)
	return err
}

func scanExecution(rows *sql.Rows) (Execution, bool) {
	var ex Execution
	var approvalID sql.NullString
	if err := rows.Scan(&ex.ID, &ex.WorkflowID, &ex.RunID, &ex.MigrationID, &ex.WorkloadID,
		&ex.TargetWeight, &ex.Status, &ex.PolicyInputHash, &approvalID, &ex.Error); err != nil {
		return Execution{}, false
	}
	ex.ApprovalID = approvalID.String
	return ex, true
}

func (p *PGStore) GetExecution(workflowID string) (Execution, bool) {
	rows, err := p.db.Query(`SELECT id::text, workflow_id, run_id, migration_id::text,
		workload_id::text, target_weight, status, policy_input_hash, approval_id::text, error
		FROM executions WHERE workflow_id = $1`, workflowID)
	if err != nil {
		return Execution{}, false
	}
	defer rows.Close()
	if rows.Next() {
		return scanExecution(rows)
	}
	return Execution{}, false
}

func (p *PGStore) GetAuditForMigration(migrationID string) []AuditEntry {
	rows, err := p.db.Query(`SELECT run_id::text, workload_id::text, actor_type, actor_id,
		action, resource, request_id, idempotency_key, policy_bundle_version,
		policy_decision, approval_id::text, result, metadata::text, created_at::text
		FROM audit_events WHERE run_id = $1::uuid ORDER BY created_at, id`, migrationID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var runID, workloadID, approvalID sql.NullString
		if err := rows.Scan(&runID, &workloadID, &e.ActorType, &e.ActorID,
			&e.Action, &e.Resource, &e.RequestID, &e.IdempotencyKey,
			&e.PolicyBundleVersion, &e.PolicyDecision, &approvalID,
			&e.Result, &e.Metadata, &e.CreatedAt); err != nil {
			continue
		}
		e.RunID, e.WorkloadID, e.ApprovalID = runID.String, workloadID.String, approvalID.String
		out = append(out, e)
	}
	return out
}

func (p *PGStore) GetExecutionsForMigration(migrationID string) []Execution {
	rows, err := p.db.Query(`SELECT id::text, workflow_id, run_id, migration_id::text,
		workload_id::text, target_weight, status, policy_input_hash, approval_id::text, error
		FROM executions WHERE migration_id = $1 ORDER BY created_at`, migrationID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Execution
	for rows.Next() {
		if ex, ok := scanExecution(rows); ok {
			out = append(out, ex)
		}
	}
	return out
}

func (p *PGStore) UpdateExecutionStatus(workflowID, status, errMsg string) error {
	_, err := p.db.Exec(`UPDATE executions SET status = $2, error = $3, updated_at = now()
		WHERE workflow_id = $1`, workflowID, status, errMsg)
	return err
}

func (p *PGStore) SaveCanaryRecord(rec CanaryRecord) error {
	baseline, err := json.Marshal(rec.Baseline)
	if err != nil {
		return err
	}
	observed, err := json.Marshal(rec.Observed)
	if err != nil {
		return err
	}
	reasons, err := json.Marshal(rec.Reasons)
	if err != nil {
		return err
	}
	_, err = p.db.Exec(`INSERT INTO canary_stages
		(id, migration_id, workload_id, stage, verdict, baseline, observed, reasons, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		rec.ID, rec.MigrationID, rec.WorkloadID, rec.Stage, rec.Verdict,
		string(baseline), string(observed), string(reasons), rec.RequestID)
	return err
}

func (p *PGStore) GetCanaryRecords(migrationID string) []CanaryRecord {
	rows, err := p.db.Query(`SELECT id::text, migration_id::text, workload_id::text,
		stage, verdict, baseline::text, observed::text, reasons::text, request_id, created_at::text
		FROM canary_stages WHERE migration_id = $1 ORDER BY created_at`, migrationID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []CanaryRecord
	for rows.Next() {
		var rec CanaryRecord
		var baselineRaw, observedRaw, reasonsRaw string
		if err := rows.Scan(&rec.ID, &rec.MigrationID, &rec.WorkloadID,
			&rec.Stage, &rec.Verdict, &baselineRaw, &observedRaw, &reasonsRaw,
			&rec.RequestID, &rec.CreatedAt); err != nil {
			continue
		}
		_ = json.Unmarshal([]byte(baselineRaw), &rec.Baseline)
		_ = json.Unmarshal([]byte(observedRaw), &rec.Observed)
		_ = json.Unmarshal([]byte(reasonsRaw), &rec.Reasons)
		if rec.Reasons == nil {
			rec.Reasons = []string{}
		}
		out = append(out, rec)
	}
	return out
}

func (p *PGStore) HasAuditRequest(requestID string) bool {
	var exists bool
	if err := p.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM audit_events WHERE request_id = $1)`, requestID).Scan(&exists); err != nil {
		return false
	}
	return exists
}

func (p *PGStore) Ping() error { return p.db.Ping() }
func (p *PGStore) Close() error { return p.db.Close() }

func (p *PGStore) GetOwnership(migrationID string) (OwnershipRecord, bool) {
	var rec OwnershipRecord
	var approvalID sql.NullString
	err := p.db.QueryRow(`SELECT migration_id::text, workload_id::text,
		current_owner, previous_owner, routing, policy_input_hash,
		approval_id::text, source_lsn, applied_lsn, lag_seconds,
		created_at::text, updated_at::text
		FROM ownership WHERE migration_id = $1`, migrationID).
		Scan(&rec.MigrationID, &rec.WorkloadID, &rec.CurrentOwner, &rec.PreviousOwner,
			&rec.Routing, &rec.PolicyInputHash, &approvalID,
			&rec.SourceLSN, &rec.AppliedLSN, &rec.LagSeconds, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		return OwnershipRecord{}, false
	}
	if approvalID.Valid {
		rec.ApprovalID = approvalID.String
	}
	return rec, true
}

func (p *PGStore) TransferOwnership(migrationID, expectOwner string, rec OwnershipRecord) (bool, error) {
	if rec.CurrentOwner != "azure" {
		return false, nil
	}
	tx, err := p.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var current string
	err = tx.QueryRow(`SELECT current_owner FROM ownership WHERE migration_id = $1`, migrationID).Scan(&current)
	switch {
	case err == sql.ErrNoRows:
		if expectOwner != "aws" {
			return false, nil
		}
		_, err = tx.Exec(`INSERT INTO ownership
			(migration_id, workload_id, current_owner, previous_owner, routing,
			 policy_input_hash, approval_id, source_lsn, applied_lsn, lag_seconds)
			VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '')::uuid, $8, $9, $10)`,
			rec.MigrationID, rec.WorkloadID, rec.CurrentOwner, rec.PreviousOwner,
			rec.Routing, rec.PolicyInputHash, rec.ApprovalID, rec.SourceLSN, rec.AppliedLSN, rec.LagSeconds)
		if err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	default:
		if current != "aws" || expectOwner != "aws" {
			return false, nil
		}
		res, err := tx.Exec(`UPDATE ownership SET current_owner = $2, previous_owner = $3,
			routing = $4, policy_input_hash = $5, approval_id = NULLIF($6, '')::uuid,
			source_lsn = $7, applied_lsn = $8, lag_seconds = $9, updated_at = now()
			WHERE migration_id = $1 AND current_owner = $10`,
			migrationID, rec.CurrentOwner, rec.PreviousOwner, rec.Routing,
			rec.PolicyInputHash, rec.ApprovalID, rec.SourceLSN, rec.AppliedLSN, rec.LagSeconds, expectOwner)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return false, nil
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
