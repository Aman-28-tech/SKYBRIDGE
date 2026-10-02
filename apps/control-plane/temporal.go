// Temporal execution skeleton (mock activities only — no cloud mutation).
//
// Boundary: authentication/authorization -> Policy Gate -> approval ->
// Temporal workflow -> activities -> (future cloud adapter) -> verification.
// Temporal NEVER authorizes: the workflow executes a pre-authorized context
// and revalidates preconditions first; any failure fails closed.
//
// Workflow identity is deterministic: same execution intent always maps to
// the same workflow ID, so repeats cannot spawn competing executions.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// actLog logs from activities both inside Temporal (structured logger) and
// in direct unit calls (activity context absent -> plain log fallback).
func actLog(ctx context.Context, msg string, kvs ...any) {
	defer func() {
		if recover() != nil {
			log.Printf("[activity] %s %v", msg, kvs)
		}
	}()
	activity.GetLogger(ctx).Info(msg, kvs...)
}

const (
	// TemporalTaskQueue carries migration execution workflows and activities.
	TemporalTaskQueue = "skybridge-migration"
	// TemporalNamespace is the v1 namespace on the local server.
	TemporalNamespace = "default"
)

// Execution statuses (bookkeeping on the Migration model, not a new lifecycle).
const (
	ExecAccepted  = "accepted"
	ExecRunning   = "running"
	ExecSucceeded = "succeeded"
	ExecFailed    = "failed"
)

// ExecutionContext is the full authorized context a workflow executes.
// Everything the workflow needs is frozen here; it never re-derives auth.
type ExecutionContext struct {
	WorkloadID            string `json:"workload_id"`
	MigrationID           string `json:"migration_id"`
	TargetProvider        string `json:"target_provider"`
	PlanID                string `json:"plan_id"`
	CompatibilityReportID string `json:"compatibility_report_id"`
	DriftReportID         string `json:"drift_report_id"`
	ApprovalID            string `json:"approval_id,omitempty"`
	PolicyBundleVersion   string `json:"policy_bundle_version"`
	PolicyInputHash       string `json:"policy_input_hash"`
	TargetWeight          int    `json:"target_weight"`
	Environment           string `json:"environment"`
	ValidationStatus      string `json:"validation_status"`
	CDCLagSeconds         int    `json:"cdc_lag_seconds"`
	TargetHealthy         bool   `json:"target_healthy"`
	WriteOwnership        string `json:"write_ownership"`
	ReadOnlyCanary        bool   `json:"read_only_canary"`
	RequestID             string `json:"request_id"`
	IdempotencyKey        string `json:"idempotency_key"`
	ActorID               string `json:"actor_id"`
	ActorType             string `json:"actor_type"`
}

// Execution is the persisted bookkeeping row for one workflow.
type Execution struct {
	ID              string `json:"id"`
	WorkflowID      string `json:"workflow_id"`
	RunID           string `json:"run_id"`
	MigrationID     string `json:"migration_id"`
	WorkloadID      string `json:"workload_id"`
	TargetWeight    int    `json:"target_weight"`
	Status          string `json:"status"`
	PolicyInputHash string `json:"policy_input_hash"`
	ApprovalID      string `json:"approval_id,omitempty"`
	Error           string `json:"error,omitempty"`
}

// ExecutionWorkflowID deterministically identifies an execution intent.
func ExecutionWorkflowID(migrationID string, targetWeight int, inputHash string) string {
	short := inputHash
	if len(short) > 12 {
		short = short[:12]
	}
	return fmt.Sprintf("migration-%s-weight-%d-%s", migrationID, targetWeight, short)
}

// TemporalClient is the seam between the API and Temporal (fake in tests).
type TemporalClient interface {
	StartExecution(ctx context.Context, workflowID string, input ExecutionContext) (runID string, err error)
	DescribeExecution(ctx context.Context, workflowID string) (status string, runID string, err error)
}

// Execution statuses from DescribeExecution.
const (
	RemoteRunning   = "running"
	RemoteCompleted = "completed"
	RemoteFailed    = "failed"
	RemoteNotFound  = "notfound"
)

func isTerminalRemote(status string) bool {
	return status == RemoteCompleted || status == RemoteFailed
}

// realTemporalClient dials a Temporal server. No cloud involved.
type realTemporalClient struct {
	c         client.Client
	namespace string
	taskQueue string
}

// NewRealTemporalClient connects to host:port (e.g. "localhost:7233").
func NewRealTemporalClient(host, namespace, taskQueue string) (TemporalClient, error) {
	c, err := client.Dial(client.Options{HostPort: host, Namespace: namespace})
	if err != nil {
		return nil, err
	}
	return &realTemporalClient{c: c, namespace: namespace, taskQueue: taskQueue}, nil
}

func (r *realTemporalClient) StartExecution(ctx context.Context, workflowID string, input ExecutionContext) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	run, err := r.c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       workflowID,
		TaskQueue:                r.taskQueue,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		WorkflowExecutionTimeout: 10 * time.Minute,
	}, MigrationStageWorkflow, input)
	if err != nil {
		return "", err
	}
	return string(run.GetRunID()), nil
}

func (r *realTemporalClient) DescribeExecution(ctx context.Context, workflowID string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	desc, err := r.c.DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		return RemoteNotFound, "", nil
	}
	info := desc.GetWorkflowExecutionInfo()
	runID := ""
	if info.GetExecution() != nil {
		runID = info.GetExecution().GetRunId()
	}
	switch info.GetStatus() {
	case enums.WORKFLOW_EXECUTION_STATUS_RUNNING:
		return RemoteRunning, runID, nil
	case enums.WORKFLOW_EXECUTION_STATUS_COMPLETED:
		return RemoteCompleted, runID, nil
	case enums.WORKFLOW_EXECUTION_STATUS_FAILED,
		enums.WORKFLOW_EXECUTION_STATUS_TERMINATED,
		enums.WORKFLOW_EXECUTION_STATUS_CANCELED,
		enums.WORKFLOW_EXECUTION_STATUS_TIMED_OUT:
		return RemoteFailed, runID, nil
	default:
		return RemoteRunning, runID, nil
	}
}

// StartWorker runs the in-process worker against host. The worker executes
// workflows/activities; it never authorizes (see package docs).
func StartWorker(tc TemporalClient, store Store) (func(), error) {
	rtc, ok := tc.(*realTemporalClient)
	if !ok {
		return nil, fmt.Errorf("worker requires a real Temporal client")
	}
	w := worker.New(rtc.c, rtc.taskQueue, worker.Options{})
	w.RegisterWorkflow(MigrationStageWorkflow)
	acts := &ExecActivities{}
	w.RegisterActivity(acts)
	if err := w.Start(); err != nil {
		return nil, err
	}
	return func() { w.Stop() }, nil
}

// activityConfig: explicit bounded retry, no infinite loops.
func activityOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 3,
		},
	})
}

// MigrationStageWorkflow executes one pre-authorized mock stage:
// accepted -> validating -> executing -> verifying -> completed.
// Any error fails closed via the deferred finalizer; later activities never
// run after a failure.
func MigrationStageWorkflow(ctx workflow.Context, input ExecutionContext) (map[string]any, error) {
	finalStatus, finalErr := ExecSucceeded, ""
	defer func() {
		dctx, _ := workflow.NewDisconnectedContext(ctx)
		_ = workflow.ExecuteActivity(activityOptions(dctx), "FinalizeExecutionActivity", map[string]any{
			"workflow_id": input.executionWorkflowID(),
			"status":      finalStatus,
			"error":       finalErr,
			"input":       input,
		}).Get(dctx, nil)
	}()

	ao := activityOptions(ctx)
	mustRecord := func(name, detail string) error {
		return workflow.ExecuteActivity(ao, "RecordActivityAuditActivity", map[string]any{
			"execution_id": input.executionWorkflowID(), "run_id": "",
			"migration_id": input.MigrationID, "workload_id": input.WorkloadID,
			"activity": name, "detail": detail, "input": input,
		}).Get(ctx, nil)
	}

	var validated map[string]any
	if err := workflow.ExecuteActivity(ao, "ValidatePreconditionsActivity", input).Get(ctx, &validated); err != nil {
		finalStatus, finalErr = ExecFailed, "validating: "+err.Error()
		return nil, fmt.Errorf("validating: %w", err)
	}
	if err := mustRecord("validating", ""); err != nil {
		finalStatus, finalErr = ExecFailed, "audit-validating: "+err.Error()
		return nil, fmt.Errorf("audit-validating: %w", err)
	}
	var planned map[string]any
	if err := workflow.ExecuteActivity(ao, "MockProvisionPlanActivity", input).Get(ctx, &planned); err != nil {
		finalStatus, finalErr = ExecFailed, "executing: "+err.Error()
		return nil, fmt.Errorf("executing: %w", err)
	}
	provisionDetail, _ := json.Marshal(map[string]any{
		"provider": planned["provider"], "operation_key": planned["operation_key"],
		"fingerprint": planned["fingerprint"], "resources": planned["components"],
		"verified": planned["verified"],
		"terraform": "none: plan-only slice, no apply attempted",
	})
	if err := mustRecord("executing", string(provisionDetail)); err != nil {
		finalStatus, finalErr = ExecFailed, "audit-executing: "+err.Error()
		return nil, fmt.Errorf("audit-executing: %w", err)
	}
	var verified map[string]any
	if err := workflow.ExecuteActivity(ao, "MockVerifyActivity", input).Get(ctx, &verified); err != nil {
		finalStatus, finalErr = ExecFailed, "verifying: "+err.Error()
		return nil, fmt.Errorf("verifying: %w", err)
	}
	if err := mustRecord("verifying", ""); err != nil {
		finalStatus, finalErr = ExecFailed, "audit-verifying: "+err.Error()
		return nil, fmt.Errorf("audit-verifying: %w", err)
	}
	if err := mustRecord("completed", ""); err != nil {
		finalStatus, finalErr = ExecFailed, "audit-completed: "+err.Error()
		return nil, fmt.Errorf("audit-completed: %w", err)
	}
	return map[string]any{"status": ExecSucceeded, "planned": planned, "verified": verified}, nil
}

func (in ExecutionContext) executionWorkflowID() string {
	return ExecutionWorkflowID(in.MigrationID, in.TargetWeight, in.PolicyInputHash)
}

// ExecActivities implements the mock-only activities. Every activity is a
// pure local computation over the control-plane store: no Terraform, no
// cloud APIs, no Kubernetes, no traffic, no CDC. Operation keys are
// deterministic (execution ID + activity name) so retries are side-effect
// free (audit writes dedupe on request_id).
type ExecActivities struct{}

// ValidatePreconditionsActivity revalidates the full evidence chain inside
// the execution boundary. Stale or denied inputs fail here, before any
// later activity runs.
func (a *ExecActivities) ValidatePreconditionsActivity(ctx context.Context, input ExecutionContext) (map[string]any, error) {
	actLog(ctx, "validate preconditions", "workflow", input.executionWorkflowID())
	runtime := map[string]any{
		"action": "shift_traffic", "environment": input.Environment,
		"target_weight": input.TargetWeight, "validation_status": input.ValidationStatus,
		"cdc_lag_seconds": input.CDCLagSeconds, "target_healthy": input.TargetHealthy,
		"write_ownership": input.WriteOwnership, "read_only_canary": input.ReadOnlyCanary,
	}
	in, fail := buildPolicyInput(input.WorkloadID, runtime, Principal{ID: input.ActorID, Type: input.ActorType})
	if fail != nil {
		return nil, temporal.NewNonRetryableApplicationError("precondition: "+fail.Code+" "+fail.Message, "PreconditionFailed", nil)
	}
	decision, reasons := policyDecide(in)
	// Fresh deny always wins: even a perfectly bound context cannot execute
	// against denying evidence. Hash mismatch is only meaningful when the
	// fresh decision would otherwise permit execution.
	if decision == PolicyDeny {
		return nil, temporal.NewNonRetryableApplicationError("precondition: policy denies ("+strings.Join(reasons, "; ")+")", "PreconditionFailed", nil)
	}
	if in.inputHash() != input.PolicyInputHash && in.eligibilityHash() != input.PolicyInputHash {
		return nil, temporal.NewNonRetryableApplicationError("precondition: policy inputs changed since authorization", "PreconditionFailed", nil)
	}
	switch decision {
	case PolicyAllow:
		return map[string]any{"decision": decision}, nil
	case PolicyApprovalRequired:
		appr, code, msg := approvalEligibleForUse(input.WorkloadID, input.ApprovalID, in)
		if appr == nil {
			return nil, temporal.NewNonRetryableApplicationError("precondition: "+code+" "+msg, "PreconditionFailed", nil)
		}
		return map[string]any{"decision": decision, "approval_id": appr.ID}, nil
	default:
		return nil, temporal.NewNonRetryableApplicationError("precondition: policy denies ("+strings.Join(reasons, "; ")+")", "PreconditionFailed", nil)
	}
}

// MockProvisionPlanActivity runs the provider-neutral adapter boundary:
// validate -> plan -> verify, all against mock-safe adapters. It creates
// nothing: the only mutating method (Apply) is never called, and every
// registered adapter refuses it unconditionally.
func (a *ExecActivities) MockProvisionPlanActivity(ctx context.Context, input ExecutionContext) (map[string]any, error) {
	plan, ok := store.GetLatestTargetPlan(input.WorkloadID)
	if !ok {
		return nil, temporal.NewNonRetryableApplicationError("mock-provision: no target plan", "PreconditionFailed", nil)
	}
	if plan.ID != input.PlanID {
		return nil, temporal.NewNonRetryableApplicationError("mock-provision: plan changed since authorization", "PreconditionFailed", nil)
	}
	adapter, err := SelectAdapter(plan.TargetProvider)
	if err != nil {
		return nil, temporal.NewNonRetryableApplicationError("mock-provision: "+err.Error(), "PreconditionFailed", nil)
	}
	req := NewProvisioningRequest(plan, adapter.Provider(), input.WorkloadID,
		input.MigrationID, input.executionWorkflowID(), input.PolicyInputHash, input.ApprovalID)
	req.Authorized = true // set only here: post-validation, inside the execution boundary
	if err := adapter.ValidateTarget(ctx, req); err != nil {
		return nil, temporal.NewNonRetryableApplicationError("mock-provision: "+err.Error(), "PreconditionFailed", nil)
	}
	prov, err := adapter.PlanInfrastructure(ctx, req)
	if err != nil {
		return nil, temporal.NewNonRetryableApplicationError("mock-provision: "+err.Error(), "PreconditionFailed", nil)
	}
	ver, err := adapter.VerifyInfrastructure(ctx, prov)
	if err != nil || !ver.Verified {
		msg := "mock-provision: verification failed"
		if err != nil {
			msg += ": " + err.Error()
		}
		return nil, temporal.NewNonRetryableApplicationError(msg, "PreconditionFailed", nil)
	}
	// Terraform plan document (validate/plan shape only; apply stays disabled).
	wlName := input.WorkloadID
	if wl, ok := store.GetWorkload(input.WorkloadID); ok {
		if n, ok := wl["name"].(string); ok && n != "" {
			wlName = n
		}
	}
	tfdoc, err := GenerateTerraformPlan(plan, adapter.Provider(), wlName)
	if err != nil {
		return nil, temporal.NewNonRetryableApplicationError("mock-provision: "+err.Error(), "PreconditionFailed", nil)
	}
	tfModules := []string{}
	for name := range tfdoc.Vars.Modules {
		tfModules = append(tfModules, name)
	}
	sort.Strings(tfModules)
	for _, r := range prov.Resources {
		actLog(ctx, "mock provision intent (no-op)", "logical_id", r.LogicalID,
			"kind", r.Kind, "mode", r.Mode, "workflow", input.executionWorkflowID())
	}
	return map[string]any{
		"components": len(prov.Resources), "mode": "mock", "mutated": false,
		"provider": prov.Provider, "operation_key": prov.OperationKey,
		"fingerprint": prov.Fingerprint, "verified": ver.Verified,
		"rpo_seconds": prov.RPOSeconds, "rto_seconds": prov.RTOSeconds,
		"tf_modules": tfModules, "tf_fingerprint": tfdoc.Fingerprint,
	}, nil
}

// MockVerifyActivity re-checks plan currency after the mock provisioning.
func (a *ExecActivities) MockVerifyActivity(ctx context.Context, input ExecutionContext) (map[string]any, error) {
	plan, ok := store.GetLatestTargetPlan(input.WorkloadID)
	if !ok || plan.ID != input.PlanID {
		return nil, temporal.NewNonRetryableApplicationError("mock-verify: plan changed since authorization", "PreconditionFailed", nil)
	}
	return map[string]any{"verified": true, "plan_id": plan.ID}, nil
}

// RecordActivityAuditActivity persists one audit row per (execution, activity).
// Idempotent: the request_id doubles as a dedupe key, so activity retries
// never duplicate audit rows.
func (a *ExecActivities) RecordActivityAuditActivity(ctx context.Context, args map[string]any) error {
	execID, _ := args["execution_id"].(string)
	name, _ := args["activity"].(string)
	migID, _ := args["migration_id"].(string)
	wlID, _ := args["workload_id"].(string)
	bundle, apprID := policyBundleVersion(), ""
	if in, ok := args["input"].(ExecutionContext); ok {
		bundle, apprID = in.PolicyBundleVersion, in.ApprovalID
	}
	reqID := execID + ":" + name
	if store.HasAuditRequest(reqID) {
		return nil
	}
	meta := map[string]any{"execution_id": execID}
	if detail, _ := args["detail"].(string); detail != "" {
		var d any
		if err := json.Unmarshal([]byte(detail), &d); err == nil {
			meta["detail"] = d
		} else {
			meta["detail"] = detail
		}
	}
	metaBytes, _ := json.Marshal(meta)
	return store.RecordAudit(AuditEntry{
		RunID: migID, WorkloadID: wlID, ActorType: "system", ActorID: "temporal-worker",
		Action: "activity:" + name, RequestID: reqID,
		PolicyBundleVersion: bundle, PolicyDecision: PolicyAllow,
		ApprovalID: apprID, Result: "success",
		Metadata: string(metaBytes),
	})
}

// FinalizeExecutionActivity marks the execution row terminal and audits it.
// Runs via disconnected context so failures still finalize (fail closed and
// visible). Idempotent through the same audit dedupe.
func (a *ExecActivities) FinalizeExecutionActivity(ctx context.Context, args map[string]any) error {
	workflowID, _ := args["workflow_id"].(string)
	status, _ := args["status"].(string)
	errMsg, _ := args["error"].(string)
	_ = store.UpdateExecutionStatus(workflowID, status, errMsg)
	exec, _ := store.GetExecution(workflowID)
	reqID := workflowID + ":finalize"
	if store.HasAuditRequest(reqID) {
		return nil
	}
	return store.RecordAudit(AuditEntry{
		RunID: exec.MigrationID, WorkloadID: exec.WorkloadID,
		ActorType: "system", ActorID: "temporal-worker",
		Action: "finalize-execution", RequestID: reqID,
		PolicyBundleVersion: policyBundleVersion(), PolicyDecision: PolicyAllow,
		ApprovalID: exec.ApprovalID, Result: "success",
		Metadata: fmt.Sprintf(`{"execution_id":%q,"status":%q,"error":%q}`, workflowID, status, errMsg),
	})
}
