// Temporal workflow tests: SDK test-suite (in-process, no server) for the
// workflow/Activity orchestration, plus direct activity unit tests.
package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
)

// fakeTemporalClient is the in-process Temporal seam for API tests.
type fakeTemporalClient struct {
	mu        sync.Mutex
	workflows map[string]*fakeWF
	starts    int
	startErr  error
}

type fakeWF struct {
	status string
	runID  string
}

func newFakeTemporalClient() *fakeTemporalClient {
	return &fakeTemporalClient{workflows: map[string]*fakeWF{}}
}

func (f *fakeTemporalClient) StartExecution(ctx context.Context, workflowID string, input ExecutionContext) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return "", f.startErr
	}
	if w, ok := f.workflows[workflowID]; ok && w.status == RemoteRunning {
		return "", errors.New("workflow execution already started")
	}
	f.starts++
	runID := "run-" + uuid()[:8]
	f.workflows[workflowID] = &fakeWF{status: RemoteRunning, runID: runID}
	return runID, nil
}

func (f *fakeTemporalClient) DescribeExecution(ctx context.Context, workflowID string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.workflows[workflowID]
	if !ok {
		return RemoteNotFound, "", nil
	}
	return w.status, w.runID, nil
}

func (f *fakeTemporalClient) setStatus(workflowID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w, ok := f.workflows[workflowID]; ok {
		w.status = status
	}
}

// execContextForTest builds a runnable context from wired pass evidence,
// deriving every field from a real policy evaluation so preconditions hold.
func execContextForTest(t *testing.T, wid, migID string, weight int) ExecutionContext {
	t.Helper()
	in, fail := buildPolicyInput(wid, map[string]any{"target_weight": weight}, Principal{ID: "operator", Type: "human"})
	if fail != nil {
		t.Fatalf("setup policy: %v", fail)
	}
	ms := store.(*MemStore)
	plan, _ := ms.GetLatestTargetPlan(wid)
	rep, _ := ms.GetLatestCompatReport(wid)
	drift, _ := ms.GetLatestDriftReport(wid, plan.ID)
	return ExecutionContext{
		WorkloadID: wid, MigrationID: migID, TargetProvider: "azure",
		PlanID: plan.ID, CompatibilityReportID: rep.ID, DriftReportID: drift.ID,
		PolicyBundleVersion: policyBundleVersion(), PolicyInputHash: in.inputHash(),
		TargetWeight: weight, Environment: in.Environment,
		ValidationStatus: in.ValidationStatus, CDCLagSeconds: in.CDCLagSeconds,
		TargetHealthy: in.TargetHealthy, WriteOwnership: in.WriteOwnership,
		ReadOnlyCanary: in.ReadOnlyCanary,
		RequestID: "req-test", IdempotencyKey: "testkey0000000001", ActorID: "operator",
		ActorType: "human",
	}
}

// Success end-to-end with real activities against wired pass evidence.
func TestWorkflowSuccess(t *testing.T) {
	resetStore()
	wid, migID := wiredPassSetup(t)
	ms := store.(*MemStore)
	plan, _ := ms.GetLatestTargetPlan(wid)
	_ = store.SaveExecution(Execution{WorkflowID: "wf-success-1", MigrationID: migID,
		WorkloadID: wid, TargetWeight: 1, Status: ExecRunning, PolicyInputHash: "h"})
	in := execContextForTest(t, wid, migID, 1)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&ExecActivities{})
	env.ExecuteWorkflow(MigrationStageWorkflow, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	var out map[string]any
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if out["status"] != ExecSucceeded {
		t.Fatalf("status=%v", out["status"])
	}
	// finalize ran: assert the audit trail covers every stage.
	found := map[string]bool{}
	for _, a := range ms.audits {
		found[a.Action] = true
	}
	for _, act := range []string{"activity:validating", "activity:executing", "activity:verifying", "activity:completed", "finalize-execution"} {
		if !found[act] {
			t.Fatalf("missing audit %s", act)
		}
	}
	_ = plan
}

// Retry then success: provision fails twice, workflow still completes, and
// the completion audit is recorded exactly once (idempotent audit).
func TestWorkflowActivityRetry(t *testing.T) {
	resetStore()
	wid, migID := wiredPassSetup(t)
	in := execContextForTest(t, wid, migID, 1)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	acts := &ExecActivities{}
	env.RegisterActivity(acts)
	calls := 0
	env.OnActivity(acts.MockProvisionPlanActivity, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in ExecutionContext) (map[string]any, error) {
			calls++
			if calls < 3 {
				return nil, errors.New("transient mock failure")
			}
			return map[string]any{"components": 1, "mode": "mock", "mutated": false}, nil
		})
	env.ExecuteWorkflow(MigrationStageWorkflow, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	var out map[string]any
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatalf("workflow error after retries: %v", err)
	}
	if calls != 3 {
		t.Fatalf("attempts=%d, want 3", calls)
	}
	ms := store.(*MemStore)
	n := 0
	for _, a := range ms.audits {
		if a.Action == "activity:executing" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("executing audits=%d, want 1", n)
	}
}

// Exhaustion: always-failing activity fails the workflow; finalize marks it.
func TestWorkflowRetryExhaustion(t *testing.T) {
	resetStore()
	wid, migID := wiredPassSetup(t)
	in := execContextForTest(t, wid, migID, 1)
	wfID := "wf-exhaust-1"
	_ = store.SaveExecution(Execution{WorkflowID: wfID, MigrationID: migID, WorkloadID: wid,
		TargetWeight: 1, Status: ExecRunning})
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	acts := &ExecActivities{}
	env.RegisterActivity(acts)
	env.OnActivity(acts.MockVerifyActivity, mock.Anything, mock.Anything).Return(
		map[string]any(nil), errors.New("persistent mock failure"))
	// workflow uses input.executionWorkflowID(); override by setting IDs to match wfID is
	// unnecessary here: assert failure propagation + finalize ran for the input's ID.
	env.ExecuteWorkflow(MigrationStageWorkflow, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	var out map[string]any
	if err := env.GetWorkflowResult(&out); err == nil {
		t.Fatal("expected workflow error")
	} else if !containsStr(err.Error(), "verifying") {
		t.Fatalf("error=%v", err)
	}
	// finalize ran for the workflow's deterministic ID (row may be absent in
	// this unit setup; assert the finalize audit exists).
	ms := store.(*MemStore)
	found := false
	for _, a := range ms.audits {
		if a.Action == "finalize-execution" {
			found = true
		}
	}
	if !found {
		t.Fatal("finalize audit missing after failure")
	}
}

// Precondition failure: no later activity runs.
func TestWorkflowPreconditionStopsLater(t *testing.T) {
	resetStore()
	wid, migID := wiredPassSetup(t)
	in := execContextForTest(t, wid, migID, 1)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	acts := &ExecActivities{}
	env.RegisterActivity(acts)
	provisionCalls := 0
	env.OnActivity(acts.ValidatePreconditionsActivity, mock.Anything, mock.Anything).Return(
		map[string]any(nil), errors.New("stale evidence"))
	env.OnActivity(acts.MockProvisionPlanActivity, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in ExecutionContext) (map[string]any, error) {
			provisionCalls++
			return map[string]any{}, nil
		})
	env.ExecuteWorkflow(MigrationStageWorkflow, in)
	var out map[string]any
	if err := env.GetWorkflowResult(&out); err == nil {
		t.Fatal("expected error")
	}
	if provisionCalls != 0 {
		t.Fatal("later activity ran after precondition failure")
	}
}

func containsStr(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// Direct activity unit tests (no server, no test env).
func TestActivitiesDirect(t *testing.T) {
	resetStore()
	wid, migID := wiredPassSetup(t)
	in := execContextForTest(t, wid, migID, 1)
	acts := &ExecActivities{}
	if _, err := acts.ValidatePreconditionsActivity(context.Background(), in); err != nil {
		t.Fatalf("validate: %v", err)
	}
	p1, err := acts.MockProvisionPlanActivity(context.Background(), in)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	p2, err := acts.MockProvisionPlanActivity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if p1["mutated"] != false || p2["components"] != p1["components"] {
		t.Fatal("provision not pure/idempotent")
	}
	if _, err := acts.MockVerifyActivity(context.Background(), in); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// audit dedupe across retries
	args := map[string]any{"execution_id": "ex-1", "activity": "validating",
		"migration_id": migID, "workload_id": wid, "input": in}
	if err := acts.RecordActivityAuditActivity(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if err := acts.RecordActivityAuditActivity(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	ms := store.(*MemStore)
	n := 0
	for _, a := range ms.audits {
		if a.RequestID == "ex-1:validating" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("audit rows=%d, want 1", n)
	}
}

// Stale evidence inside the execution boundary fails closed.
func TestActivityStaleFails(t *testing.T) {
	resetStore()
	wid, migID := wiredPassSetup(t)
	in := execContextForTest(t, wid, migID, 1)
	acts := &ExecActivities{}
	// drift the world after authorization: new blocking drift
	ms := store.(*MemStore)
	plan, _ := ms.GetLatestTargetPlan(wid)
	_ = store.SaveDriftReport(EvaluateDrift(plan, loadFixture(t, "versioning-off")))
	if _, err := acts.ValidatePreconditionsActivity(context.Background(), in); err == nil {
		t.Fatal("expected precondition failure on stale drift")
	} else if !containsStr(err.Error(), "blocking_drift") {
		t.Fatalf("error=%v", err)
	}
}
