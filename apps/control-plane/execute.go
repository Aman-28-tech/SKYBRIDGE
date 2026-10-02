// Execution API: Temporal-backed stage execution behind authorization.
//
// Flow per request: authenticate (existing) -> buildPolicyInput (stored
// evidence) -> policyDecide -> approval iff gated -> execution guard
// (no competing execution) -> Temporal start. Temporal never authorizes;
// the workflow revalidates preconditions before any activity runs.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// temporalClient is set when TEMPORAL_HOST is configured (fail-closed:
// execution without a reachable execution plane is refused).
var temporalClient TemporalClient

func temporalCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// postExecute implements POST /v1/migrations/{id}/execute.
func postExecute(w http.ResponseWriter, r *http.Request, migrationID string) {
	rid := reqID(r)
	actor, ok := requireAuth(w, r)
	if !ok {
		return
	}
	// Caller -> principal -> workload -> migration -> mutation (H-2), with
	// explicit migration -> workload binding. Cross-migration execution
	// (an approval or execution context from another migration/workload) is
	// rejected: approvalEligibleForUse re-scopes the approval to this
	// workload below.
	wid, ok := requireMigrationAccess(w, r, actor, migrationID, "")
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 {
		writeErr(w, rid, "VALIDATION_FAILED", "Idempotency-Key required (min 16 chars)", 400)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "unreadable body", 400)
		return
	}
	hash, err := canonicalHash(raw)
	if err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "invalid JSON", 400)
		return
	}
	var runtime map[string]any
	if err := json.Unmarshal(raw, &runtime); err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "invalid JSON", 400)
		return
	}
	if _, present := runtime["target_weight"]; !present {
		writeErr(w, rid, "VALIDATION_FAILED", "target_weight required", 400)
		return
	}
	mu := idemLock(key)
	mu.Lock()
	defer mu.Unlock()
	if resp, status, storedHash, found := store.CheckIdem(key); found {
		if storedHash == "" || storedHash == hash {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(resp)
			return
		}
		writeErr(w, rid, "IDEMPOTENCY_CONFLICT", "same Idempotency-Key with different request body", 409)
		return
	}
	if temporalClient == nil {
		writeErr(w, rid, "TEMPORAL_UNAVAILABLE", "execution plane not configured", 503)
		return
	}
	// Authorization: identical semantics to cutover eligibility.
	pin, fail := buildPolicyInput(wid, runtime, actor)
	if fail != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fail.Status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": fail.Code, "message": fail.Message,
			"request_id": rid, "details": map[string]any{}}})
		return
	}
	decision, reasons := policyDecide(pin)
	var approvalID string
	switch decision {
	case PolicyAllow:
	case PolicyApprovalRequired:
		aid, _ := runtime["approval_id"].(string)
		appr, code, msg := approvalEligibleForUse(wid, aid, pin)
		if appr == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": code, "message": msg, "request_id": rid,
				"details": map[string]any{"decision": decision}}})
			return
		}
		approvalID = appr.ID
	default:
		reason := strings.Join(reasons, "; ")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "EXECUTION_DENIED", "message": reason, "request_id": rid,
			"details": map[string]any{"deny_reason": reason, "decision": decision}}})
		return
	}
	workflowID := ExecutionWorkflowID(migrationID, pin.TargetWeight, pin.inputHash())
	// Concurrency guard: heal stale rows, refuse live competitors.
	ctx, cancel := temporalCtx()
	defer cancel()
	for _, ex := range store.GetExecutionsForMigration(migrationID) {
		if ex.Status != ExecRunning {
			continue
		}
		status, _, err := temporalClient.DescribeExecution(ctx, ex.WorkflowID)
		if err != nil || status == RemoteRunning {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": "EXECUTION_IN_FLIGHT",
				"message": "another execution owns this migration: " + ex.WorkflowID,
				"request_id": rid, "details": map[string]any{"workflow_id": ex.WorkflowID}}})
			return
		}
		if isTerminalRemote(status) {
			mapped := ExecSucceeded
			if status == RemoteFailed {
				mapped = ExecFailed
			}
			_ = store.UpdateExecutionStatus(ex.WorkflowID, mapped, "healed by execution guard")
		}
	}
	input := ExecutionContext{
		WorkloadID: wid, MigrationID: migrationID, TargetProvider: "azure",
		PlanID: planIDFor(wid), CompatibilityReportID: compatIDFor(wid), DriftReportID: driftIDFor(wid),
		ApprovalID: approvalID, PolicyBundleVersion: policyBundleVersion(),
		PolicyInputHash: pin.inputHash(), TargetWeight: pin.TargetWeight,
		Environment: pin.Environment, ValidationStatus: pin.ValidationStatus,
		CDCLagSeconds: pin.CDCLagSeconds, TargetHealthy: pin.TargetHealthy,
		WriteOwnership: pin.WriteOwnership, ReadOnlyCanary: pin.ReadOnlyCanary,
		RequestID: rid, IdempotencyKey: key, ActorID: actor.ID, ActorType: actor.Type,
	}
	runID, err := temporalClient.StartExecution(ctx, workflowID, input)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "TEMPORAL_UNAVAILABLE", "message": "could not start execution: " + err.Error(),
			"request_id": rid, "details": map[string]any{}}})
		return
	}
	_ = store.SaveExecution(Execution{
		WorkflowID: workflowID, RunID: runID, MigrationID: migrationID, WorkloadID: wid,
		TargetWeight: pin.TargetWeight, Status: ExecRunning,
		PolicyInputHash: pin.inputHash(), ApprovalID: approvalID,
	})
	_ = store.RecordAudit(AuditEntry{
		RunID: migrationID, WorkloadID: wid, ActorType: actor.Type, ActorID: actor.ID,
		Action: "start_execution", RequestID: rid, IdempotencyKey: key,
		PolicyBundleVersion: policyBundleVersion(), PolicyDecision: decision, Result: "success",
		ApprovalID: approvalID,
		Metadata: fmt.Sprintf(`{"policy_input_hash":%q,"workflow_id":%q,"run_id":%q}`, pin.inputHash(), workflowID, runID),
	})
	resp, _ := json.Marshal(map[string]any{
		"workflow_id": workflowID, "run_id": runID, "migration_id": migrationID,
		"status": ExecRunning, "request_id": rid, "decision": decision,
		"policy_bundle_version": policyBundleVersion(), "policy_input_hash": pin.inputHash(),
	})
	store.SaveIdem(key, hash, resp, 202)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(202)
	_, _ = w.Write(resp)
}

// getExecution implements GET /v1/migrations/{id}/execution (latest + live status).
func getExecution(w http.ResponseWriter, r *http.Request, migrationID string) {
	rid := reqID(r)
	if _, ok := store.GetMigration(migrationID); !ok {
		writeErr(w, rid, "NOT_FOUND", "migration not found", 404)
		return
	}
	rows := store.GetExecutionsForMigration(migrationID)
	if len(rows) == 0 {
		writeErr(w, rid, "NOT_FOUND", "no execution yet for migration", 404)
		return
	}
	latest := rows[len(rows)-1]
	live := latest.Status
	var liveRun string
	if temporalClient != nil && latest.Status == ExecRunning {
		ctx, cancel := temporalCtx()
		defer cancel()
		if status, runID, err := temporalClient.DescribeExecution(ctx, latest.WorkflowID); err == nil {
			liveRun = runID
			switch status {
			case RemoteRunning:
				live = ExecRunning
			case RemoteCompleted:
				live = ExecSucceeded
			case RemoteFailed:
				live = ExecFailed
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"workflow_id": latest.WorkflowID, "run_id": latest.RunID,
		"migration_id": migrationID, "status": latest.Status, "live_status": live,
		"live_run_id": liveRun, "target_weight": latest.TargetWeight,
	})
}

// planIDFor/compatIDFor/driftIDFor expose current evidence IDs for context building.
func planIDFor(workloadID string) string {
	plan, ok := store.GetLatestTargetPlan(workloadID)
	if !ok {
		return ""
	}
	return plan.ID
}

func compatIDFor(workloadID string) string {
	rep, ok := store.GetLatestCompatReport(workloadID)
	if !ok {
		return ""
	}
	return rep.ID
}

func driftIDFor(workloadID string) string {
	plan, ok := store.GetLatestTargetPlan(workloadID)
	if !ok {
		return ""
	}
	rep, ok := store.GetLatestDriftReport(workloadID, plan.ID)
	if !ok {
		return ""
	}
	return rep.ID
}
