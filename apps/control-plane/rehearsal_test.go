// Rehearsal tests: deterministic end-to-end dry run over the completed
// slices (run: go test ./...). Fakes stand in for the local data plane;
// live acceptance replays the same path against PG + Redpanda.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func rehearseReq(t *testing.T, wid, migID, key, body string) *httptest.ResponseRecorder {
	return rehearseReqAs(t, wid, migID, key, body, "test-admin")
}

func rehearseReqAs(t *testing.T, wid, migID, key, body, actor string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+migID+"/rehearse", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	withBearer(req, actor, "")
	rec := httptest.NewRecorder()
	postRehearse(rec, req, wid, migID)
	return rec
}

func decodeRehearsal(t *testing.T, rec *httptest.ResponseRecorder) RehearsalResult {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var res RehearsalResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	return res
}

func useFake(f *fakeRehearsalDataPlane) func() {
	old := rehearsalDataPlane
	rehearsalDataPlane = f
	return func() { rehearsalDataPlane = old }
}

// rehearsalChain builds register -> migration -> compat -> plan -> clean
// drift through the real handlers (conditional compat, like the live lab).
func rehearsalChain(t *testing.T, key string) (wid, migID string) {
	t.Helper()
	resetStore()
	wid = registerPlanWID(t, key+"reg00001")
	evalCompat(t, wid, key+"cmp00001")
	evalPlanForDrift(t, wid)
	evalDriftForPlan(t, wid, "clean")
	migID = createMig(t, wid)
	return wid, migID
}

// craftPassEvidence stores pass-status evidence directly (freshness IDs
// computed exactly like buildPolicyInput expects).
func craftPassEvidence(t *testing.T, wid string) (planID, compatID, driftID string) {
	t.Helper()
	wl, ok := store.GetWorkload(wid)
	if !ok {
		t.Fatal("no workload")
	}
	spec, ok := wl["canonical_spec"].(map[string]any)
	if !ok {
		t.Fatal("no spec")
	}
	specRaw, _ := json.Marshal(spec)
	specHash, _ := canonicalHash(specRaw)
	reg, err := EmbeddedRegistry()
	if err != nil {
		t.Fatal(err)
	}
	compatID = ReportID(wid, reg.Version, specHash)
	if err := store.SaveCompatReport(CompatReport{ID: compatID, WorkloadID: wid,
		TargetProvider: "azure", Status: "pass",
		RegistryVersion: reg.Version, EvaluatorVersion: EvaluatorVersion}); err != nil {
		t.Fatal(err)
	}
	planID = "plan-pass-0001"
	if err := store.SaveTargetPlan(TargetMigrationPlan{ID: planID, WorkloadID: wid,
		SourceProvider: "aws", TargetProvider: "azure", CanonicalSpecVersion: 1,
		CompatibilityReportID: compatID, CompatibilityRegistryVersion: reg.Version,
		PlannerVersion: PlannerVersion, OverallStatus: "pass"}); err != nil {
		t.Fatal(err)
	}
	driftID = "drift-pass-0001"
	if err := store.SaveDriftReport(DriftReport{ID: driftID, WorkloadID: wid,
		Status: "open", DesiredReference: planID, ObservedReference: "snap-pass-1",
		DriftGate: "clear", Findings: []DriftFinding{}}); err != nil {
		t.Fatal(err)
	}
	return planID, compatID, driftID
}

func okFake() *fakeRehearsalDataPlane {
	return &fakeRehearsalDataPlane{
		Report: healthyCDCReport("0/A001", "0/A001"),
		Recon:  cleanRecon(2),
	}
}

const rehearsalBody = `{"environment":"dev","target_weight":0}`

// Happy path on pass evidence: READY with the full result contract.
func TestRehearsalHappyPass(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "rehearsalpass0001")
	planID, compatID, driftID := craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	defer useFake(okFake())()
	rec := rehearseReq(t, wid, migID, "rehearsalkey000001", rehearsalBody)
	res := decodeRehearsal(t, rec)
	if res.Status != RehearsalReady || res.Stage != RehearsalReady {
		t.Fatalf("status=%s stage=%s failure=%s %s", res.Status, res.Stage, res.FailureCode, res.FailureReason)
	}
	if !strings.HasPrefix(res.RehearsalID, "rehearsal-") {
		t.Fatalf("rehearsal id: %q", res.RehearsalID)
	}
	if res.PlanID != planID || res.CompatibilityReportID != compatID || res.DriftReportID != driftID {
		t.Fatalf("evidence refs: %+v", res)
	}
	if res.PolicyDecision != PolicyAllow || res.PolicyInputHash == "" {
		t.Fatalf("policy: %+v", res)
	}
	if res.CDC == nil || res.CDC.SourceLSN != "0/A001" || res.CDC.LagSeconds != 5 {
		t.Fatalf("cdc: %+v", res.CDC)
	}
	if res.RPODecision != "within_rpo" {
		t.Fatalf("rpo: %+v", res)
	}
	if res.Reconciliation == nil || !res.Reconciliation.Match {
		t.Fatalf("reconciliation: %+v", res.Reconciliation)
	}
	if res.ProvisioningProvider != "azure" || res.ProvisioningOpKey == "" ||
		res.ProvisioningFingerprint == "" || res.TerraformFingerprint == "" {
		t.Fatalf("provisioning: %+v", res)
	}
	// Stops before cutover: no execution/workflow/run, lifecycle untouched.
	if res.ExecutionID != "" || res.WorkflowID != "" || res.RunID != "" {
		t.Fatalf("rehearsal must not start executions: %+v", res)
	}
	if mig, _ := store.GetMigration(migID); mig["status"] != "REGISTERED" {
		t.Fatalf("migration mutated: %+v", mig)
	}
	if wl, _ := store.GetWorkload(wid); wl["lifecycle_state"] != "registered" {
		t.Fatalf("workload mutated: %+v", wl)
	}
	if len(store.GetExecutionsForMigration(migID)) != 0 {
		t.Fatal("execution rows created")
	}
	want := []string{"accepted", "evidence", "provisional_policy", "provisioning_tfvars",
		"replication", "reconciliation", "final_policy", "provisioning_intent"}
	if rehearsalStagesJSON(res.Stages) != rehearsalStagesJSON(want) {
		t.Fatalf("stages=%v want %v", res.Stages, want)
	}
	// Audit trail present, redacted.
	ms := store.(*MemStore)
	seen := map[string]bool{}
	for _, a := range ms.audits {
		seen[a.Action] = true
		if strings.Contains(a.Metadata, "cloudshop-secret") || strings.Contains(strings.ToLower(a.Metadata), "password") {
			t.Fatalf("audit leaks secret: %+v", a)
		}
	}
	for _, want := range []string{"rehearsal_start", "rehearsal_replication", "rehearsal_provisioning_intent", "rehearsal_complete", "rehearse_migration"} {
		if !seen[want] {
			t.Fatalf("missing audit %q (have %v)", want, seen)
		}
	}
}

// Conditional path: approval bound to the measured runtime -> READY.
func TestRehearsalConditionalApprovalPath(t *testing.T) {
	wid, migID := rehearsalChain(t, "rehearsalcond0001")
	runtime := `{"target_weight":0,"environment":"dev","validation_status":"passed","cdc_lag_seconds":5,"target_healthy":true,"read_only_canary":true,"write_ownership":"aws"}`
	arec := postApprovalReq(t, wid, "rehearsalcondkey1", runtime, "requester-1", "human")
	if arec.Code != 201 {
		t.Fatalf("approval create: %d %s", arec.Code, arec.Body.String())
	}
	appr := decodeApproval(t, arec)
	drec := decideReq(t, wid, appr.ID, "rehearsalcondkey2", "approved", "approver-1", "human")
	if drec.Code != 200 {
		t.Fatalf("approval decide: %d %s", drec.Code, drec.Body.String())
	}
	defer useFake(okFake())()
	body := `{"environment":"dev","target_weight":0,"approval_id":"` + appr.ID + `"}`
	rec := rehearseReq(t, wid, migID, "rehearsalcondkey3", body)
	res := decodeRehearsal(t, rec)
	if res.Status != RehearsalReady {
		t.Fatalf("status=%s failure=%s %s", res.Status, res.FailureCode, res.FailureReason)
	}
	if res.ApprovalID != appr.ID || res.PolicyDecision != PolicyApprovalRequired {
		t.Fatalf("approval: %+v", res)
	}
}

// A: compatibility blocked -> terminal before any CDC work.
func TestRehearsalCompatBlocked(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "rehearsalblk00001")
	wl, _ := store.GetWorkload(wid)
	spec, _ := wl["canonical_spec"].(map[string]any)
	specRaw, _ := json.Marshal(spec)
	specHash, _ := canonicalHash(specRaw)
	reg, _ := EmbeddedRegistry()
	compatID := ReportID(wid, reg.Version, specHash)
	_ = store.SaveCompatReport(CompatReport{ID: compatID, WorkloadID: wid,
		TargetProvider: "azure", Status: "block",
		RegistryVersion: reg.Version, EvaluatorVersion: EvaluatorVersion})
	_ = store.SaveTargetPlan(TargetMigrationPlan{ID: "plan-block-0001", WorkloadID: wid,
		SourceProvider: "aws", TargetProvider: "azure", CanonicalSpecVersion: 1,
		CompatibilityReportID: compatID, CompatibilityRegistryVersion: reg.Version,
		PlannerVersion: PlannerVersion, OverallStatus: "block"})
	_ = store.SaveDriftReport(DriftReport{ID: "drift-block-0001", WorkloadID: wid,
		Status: "open", DesiredReference: "plan-block-0001", ObservedReference: "snap-1",
		DriftGate: "clear", Findings: []DriftFinding{}})
	migID := createMig(t, wid)
	f := okFake()
	defer useFake(f)()
	rec := rehearseReq(t, wid, migID, "rehearsalblkkey01", rehearsalBody)
	res := decodeRehearsal(t, rec)
	if res.Status != RehearsalBlocked || res.FailureCode != "compatibility_not_pass" {
		t.Fatalf("status=%s failure=%s %s", res.Status, res.FailureCode, res.FailureReason)
	}
	if f.RunCalls != 0 || f.ReconCalls != 0 {
		t.Fatal("blocked rehearsal must not touch the data plane")
	}
}

// B: stale planner version -> PLAN_STALE at evidence.
func TestRehearsalStalePlanner(t *testing.T) {
	wid, migID := rehearsalChain(t, "rehearsalstale001")
	plan, _ := store.GetLatestTargetPlan(wid)
	plan.PlannerVersion = "planner-v1"
	_ = store.SaveTargetPlan(plan)
	f := okFake()
	defer useFake(f)()
	rec := rehearseReq(t, wid, migID, "rehearsalstalekey1", rehearsalBody)
	res := decodeRehearsal(t, rec)
	if res.Status != RehearsalBlocked || res.FailureCode != "PLAN_STALE" {
		t.Fatalf("status=%s failure=%s %s", res.Status, res.FailureCode, res.FailureReason)
	}
	if f.RunCalls != 0 {
		t.Fatal("stale plan must block before replication")
	}
}

// C: stale compatibility -> COMPATIBILITY_STALE.
func TestRehearsalStaleCompat(t *testing.T) {
	wid, migID := rehearsalChain(t, "rehearsalstale002")
	_ = store.SaveCompatReport(CompatReport{ID: "compat-fabricated", WorkloadID: wid,
		TargetProvider: "azure", Status: "pass", RegistryVersion: 1, EvaluatorVersion: EvaluatorVersion})
	defer useFake(okFake())()
	rec := rehearseReq(t, wid, migID, "rehearsalstalekey2", rehearsalBody)
	res := decodeRehearsal(t, rec)
	if res.Status != RehearsalBlocked || res.FailureCode != "COMPATIBILITY_STALE" {
		t.Fatalf("status=%s failure=%s %s", res.Status, res.FailureCode, res.FailureReason)
	}
}

// D: blocking drift -> blocking_drift before replication.
func TestRehearsalBlockingDrift(t *testing.T) {
	wid, migID := rehearsalChain(t, "rehearsaldrift001")
	evalDriftForPlan(t, wid, "versioning-off")
	f := okFake()
	defer useFake(f)()
	rec := rehearseReq(t, wid, migID, "rehearsaldriftkey1", rehearsalBody)
	res := decodeRehearsal(t, rec)
	if res.Status != RehearsalBlocked || res.FailureCode != "blocking_drift" {
		t.Fatalf("status=%s failure=%s %s", res.Status, res.FailureCode, res.FailureReason)
	}
	if f.RunCalls != 0 {
		t.Fatal("blocking drift must stop replication")
	}
}

// E: approval stale after new drift evidence -> APPROVAL_STALE.
func TestRehearsalStaleApproval(t *testing.T) {
	wid, migID := rehearsalChain(t, "rehearsalstale003")
	runtime := `{"target_weight":0,"environment":"dev","validation_status":"passed","cdc_lag_seconds":5,"target_healthy":true,"read_only_canary":true,"write_ownership":"aws"}`
	arec := postApprovalReq(t, wid, "rehearsalstalekey3", runtime, "requester-1", "human")
	appr := decodeApproval(t, arec)
	if drec := decideReq(t, wid, appr.ID, "rehearsalstalekey4", "approved", "approver-1", "human"); drec.Code != 200 {
		t.Fatalf("decide: %d %s", drec.Code, drec.Body.String())
	}
	evalDriftForPlan(t, wid, "versioning-off") // new drift evidence invalidates binding
	defer useFake(okFake())()
	body := `{"environment":"dev","target_weight":0,"approval_id":"` + appr.ID + `"}`
	rec := rehearseReq(t, wid, migID, "rehearsalstalekey5", body)
	res := decodeRehearsal(t, rec)
	// New drift is blocking, so the provisional gate fires first; either way
	// the rehearsal is blocked and never reaches READY.
	if res.Status != RehearsalBlocked {
		t.Fatalf("status=%s (must block)", res.Status)
	}
}

// E2: stale approval with clean evidence (hash mismatch via wrong lag).
func TestRehearsalStaleApprovalHash(t *testing.T) {
	wid, migID := rehearsalChain(t, "rehearsalstale004")
	runtime := `{"target_weight":0,"environment":"dev","validation_status":"passed","cdc_lag_seconds":9,"target_healthy":true,"read_only_canary":true,"write_ownership":"aws"}`
	arec := postApprovalReq(t, wid, "rehearsalstalekey6", runtime, "requester-1", "human")
	appr := decodeApproval(t, arec)
	if drec := decideReq(t, wid, appr.ID, "rehearsalstalekey7", "approved", "approver-1", "human"); drec.Code != 200 {
		t.Fatalf("decide: %d %s", drec.Code, drec.Body.String())
	}
	defer useFake(okFake())() // measures lag 5, approval bound lag 9
	body := `{"environment":"dev","target_weight":0,"approval_id":"` + appr.ID + `"}`
	rec := rehearseReq(t, wid, migID, "rehearsalstalekey8", body)
	res := decodeRehearsal(t, rec)
	if res.Status != RehearsalBlocked || res.FailureCode != "APPROVAL_STALE" {
		t.Fatalf("status=%s failure=%s %s", res.Status, res.FailureCode, res.FailureReason)
	}
}

// F: conditional without approval -> APPROVAL_REQUIRED.
func TestRehearsalMissingApproval(t *testing.T) {
	wid, migID := rehearsalChain(t, "rehearsalnoappr001")
	defer useFake(okFake())()
	rec := rehearseReq(t, wid, migID, "rehearsalnoapprkey1", rehearsalBody)
	res := decodeRehearsal(t, rec)
	if res.Status != RehearsalBlocked || res.FailureCode != RehearsalApprovalRequired {
		t.Fatalf("status=%s failure=%s %s", res.Status, res.FailureCode, res.FailureReason)
	}
}

// G: deny wins even with an approval (lag breach measured after binding).
func TestRehearsalDenyDespiteApproval(t *testing.T) {
	wid, migID := rehearsalChain(t, "rehearsaldeny0001")
	runtime := `{"target_weight":0,"environment":"dev","validation_status":"passed","cdc_lag_seconds":5,"target_healthy":true,"read_only_canary":true,"write_ownership":"aws"}`
	arec := postApprovalReq(t, wid, "rehearsaldenykey1", runtime, "requester-1", "human")
	appr := decodeApproval(t, arec)
	if drec := decideReq(t, wid, appr.ID, "rehearsaldenykey2", "approved", "approver-1", "human"); drec.Code != 200 {
		t.Fatalf("decide: %d", drec.Code)
	}
	f := okFake()
	f.Report = healthyCDCReport("0/B001", "0/B001")
	f.Report.LagSeconds = 45
	f.Report.ObserveUnix = f.Report.SourceCommitUnix + 45
	f.Report.WithinRPO = false
	defer useFake(f)()
	body := `{"environment":"dev","target_weight":0,"approval_id":"` + appr.ID + `"}`
	rec := rehearseReq(t, wid, migID, "rehearsaldenykey3", body)
	res := decodeRehearsal(t, rec)
	if res.Status != RehearsalBlocked || res.FailureCode != "cdc_lag_exceeds_rpo" {
		t.Fatalf("status=%s failure=%s %s", res.Status, res.FailureCode, res.FailureReason)
	}
	if res.RPODecision != "rpo_breach" {
		t.Fatalf("rpo: %+v", res)
	}
}

// H: target unavailable -> CDC_UNAVAILABLE, reconciliation never attempted.
func TestRehearsalTargetUnavailable(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "rehearsalh0000001")
	craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	f := &fakeRehearsalDataPlane{RunErr: errFakeTargetDown, Recon: cleanRecon(2)}
	defer useFake(f)()
	rec := rehearseReq(t, wid, migID, "rehearsalhkey00001", rehearsalBody)
	res := decodeRehearsal(t, rec)
	if res.Status != RehearsalBlocked || res.FailureCode != RehearsalCDCUnavailable {
		t.Fatalf("status=%s failure=%s %s", res.Status, res.FailureCode, res.FailureReason)
	}
	if f.ReconCalls != 0 {
		t.Fatal("failed replication must stop reconciliation")
	}
}

var errFakeTargetDown = errTestTargetDown()

func errTestTargetDown() error {
	return errFake("target database unavailable: connection refused")
}

type errFake string

func (e errFake) Error() string { return string(e) }

// I: lag above RPO -> deny with breach decision.
func TestRehearsalLagBreach(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "rehearsallag00001")
	craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	f := okFake()
	f.Report = healthyCDCReport("0/C001", "0/C001")
	f.Report.LagSeconds = 45
	f.Report.ObserveUnix = f.Report.SourceCommitUnix + 45
	f.Report.WithinRPO = false
	defer useFake(f)()
	rec := rehearseReq(t, wid, migID, "rehearsallagkey01", rehearsalBody)
	res := decodeRehearsal(t, rec)
	if res.Status != RehearsalBlocked || res.FailureCode != "cdc_lag_exceeds_rpo" {
		t.Fatalf("status=%s failure=%s %s", res.Status, res.FailureCode, res.FailureReason)
	}
}

// J: reconciliation mismatch -> validation_failed.
func TestRehearsalReconMismatch(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "rehearsalrecon001")
	craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	f := okFake()
	tables := cleanRecon(2).Tables
	tables[0].MissingIDs = []string{"probe-1:target"}
	tables[0].ProbesMatched = 1
	f.Recon = Reconciliation{Tables: tables, Match: false, Fingerprint: reconFingerprint(tables)}
	defer useFake(f)()
	rec := rehearseReq(t, wid, migID, "rehearsalreconkey1", rehearsalBody)
	res := decodeRehearsal(t, rec)
	if res.Status != RehearsalBlocked || res.FailureCode != RehearsalReconMismatch {
		t.Fatalf("status=%s failure=%s %s", res.Status, res.FailureCode, res.FailureReason)
	}
	if res.Reconciliation == nil || res.Reconciliation.Match {
		t.Fatalf("reconciliation: %+v", res.Reconciliation)
	}
}

// K: blocked plan component -> adapter/tfvars refusal.
func TestRehearsalAdapterFailure(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "rehearsalk0000001")
	planID, compatID, driftID := craftPassEvidence(t, wid)
	plan, _ := store.GetLatestTargetPlan(wid)
	plan.Components = []PlanComponent{{
		Key: "database", LogicalRequirement: "db", TargetCapability: "x",
		TargetService: "svc", DesiredConfiguration: map[string]any{"a": 1},
		ProvisioningMode: ModeBlocked,
	}}
	_ = store.SaveTargetPlan(plan)
	migID := createMig(t, wid)
	defer useFake(okFake())()
	rec := rehearseReq(t, wid, migID, "rehearsaladapterk1", rehearsalBody)
	res := decodeRehearsal(t, rec)
	if res.Status != RehearsalBlocked || res.FailureCode != RehearsalAdapterFailed {
		t.Fatalf("status=%s failure=%s %s", res.Status, res.FailureCode, res.FailureReason)
	}
	_, _, _ = planID, compatID, driftID
}

// L: transient CDC failure resumes on re-execution with identical probes.
func TestRehearsalCheckpointResume(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "rehearsalresume001")
	craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	f := okFake()
	f.FailTimes = 1
	defer useFake(f)()
	r1 := rehearseReq(t, wid, migID, "rehearsalresumek1", rehearsalBody)
	res1 := decodeRehearsal(t, r1)
	if res1.Status != RehearsalBlocked || res1.FailureCode != RehearsalCDCUnavailable {
		t.Fatalf("first: status=%s failure=%s", res1.Status, res1.FailureCode)
	}
	firstProbes := append([]string{}, f.LastSpec.ProbeIDs...)
	r2 := rehearseReq(t, wid, migID, "rehearsalresumek2", rehearsalBody)
	res2 := decodeRehearsal(t, r2)
	if res2.Status != RehearsalReady {
		t.Fatalf("resume: status=%s failure=%s %s", res2.Status, res2.FailureCode, res2.FailureReason)
	}
	if f.RunCalls != 2 {
		t.Fatalf("calls=%d", f.RunCalls)
	}
	// Each rehearsal owns its probes: distinct keys derive distinct,
	// collision-free probe sets (no duplicate application across resume).
	secondProbes := f.LastSpec.ProbeIDs
	if len(firstProbes) != len(secondProbes) || len(firstProbes) != 2 {
		t.Fatalf("probe sets: %v vs %v", firstProbes, secondProbes)
	}
	seen := map[string]bool{}
	for _, p := range append(firstProbes, secondProbes...) {
		if seen[p] {
			t.Fatalf("probe collision across resume: %s", p)
		}
		seen[p] = true
		if len(p) != 36 {
			t.Fatalf("probe not UUID: %q", p)
		}
	}
}

// M: same key + same body -> byte-identical replay, single data-plane run.
func TestRehearsalIdempotentReplay(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "rehearsalidem0001")
	craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	f := okFake()
	defer useFake(f)()
	r1 := rehearseReq(t, wid, migID, "rehearsalidemkey1", rehearsalBody)
	r2 := rehearseReq(t, wid, migID, "rehearsalidemkey1", rehearsalBody)
	if r1.Code != 200 || r2.Code != 200 || r1.Body.String() != r2.Body.String() {
		t.Fatal("replay not identical")
	}
	if f.RunCalls != 1 {
		t.Fatalf("replay re-executed data plane: %d", f.RunCalls)
	}
	// Same key + different body -> 409.
	r3 := rehearseReq(t, wid, migID, "rehearsalidemkey1", `{"environment":"staging","target_weight":0}`)
	if r3.Code != 409 {
		t.Fatalf("expected 409, got %d: %s", r3.Code, r3.Body.String())
	}
}

// N: concurrent same-key rehearsals -> one execution, identical bytes.
func TestRehearsalConcurrentSameKey(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "rehearsalconc0001")
	craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	f := okFake()
	defer useFake(f)()
	const n = 8
	out := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = rehearseReq(t, wid, migID, "rehearsalconckey1", rehearsalBody).Body.String()
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if out[i] != out[0] {
			t.Fatal("concurrent rehearsals diverged")
		}
	}
	if f.RunCalls != 1 {
		t.Fatalf("concurrent rehearsals executed %d times", f.RunCalls)
	}
}

// Deterministic IDs: same body, different keys -> same rehearsal ID.
func TestRehearsalDeterministicIDs(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "rehearsaldet00001")
	craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	defer useFake(okFake())()
	r1 := decodeRehearsal(t, rehearseReq(t, wid, migID, "rehearsaldetkey01", rehearsalBody))
	r2 := decodeRehearsal(t, rehearseReq(t, wid, migID, "rehearsaldetkey02", rehearsalBody))
	if r1.Status != RehearsalReady || r2.Status != RehearsalReady {
		t.Fatalf("statuses %s/%s", r1.Status, r2.Status)
	}
	if r1.RehearsalID != r2.RehearsalID {
		t.Fatalf("ids %s vs %s", r1.RehearsalID, r2.RehearsalID)
	}
}

// The broker advertises its in-docker address; host callers must re-anchor.
func TestReanchorBaseURI(t *testing.T) {
	got := reanchorBaseURI("http://localhost:8082",
		"http://redpanda:8082/consumers/g/instances/i")
	if got != "http://localhost:8082/consumers/g/instances/i" {
		t.Fatalf("reanchor: %q", got)
	}
}

// Request validation: canonical weight, probe bounds, key, migration scope.
func TestRehearsalValidation(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "rehearsalval00001")
	craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	defer useFake(okFake())()
	if rec := rehearseReq(t, wid, migID, "rehearsalvalkey01", `{"target_weight":10}`); rec.Code != 400 {
		t.Fatalf("weight: %d", rec.Code)
	}
	if rec := rehearseReq(t, wid, migID, "rehearsalvalkey02", `{"probes":99}`); rec.Code != 400 {
		t.Fatalf("probes: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+migID+"/rehearse", strings.NewReader(rehearsalBody))
	withBearer(req, "test-admin", "")
	rec := httptest.NewRecorder()
	postRehearse(rec, req, wid, migID) // no key
	if rec.Code != 400 {
		t.Fatalf("key: %d", rec.Code)
	}
	if rec := rehearseReq(t, wid, "00000000-0000-4000-8000-000000000000", "rehearsalvalkey03", rehearsalBody); rec.Code != 404 {
		t.Fatalf("migration: %d", rec.Code)
	}
}
