// Final cutover tests: ownership transfer, split-brain safety, rollback
// blocking, concurrency (run: go test ./...).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func cutoverFinalReq(t *testing.T, wid, migID, key, body string) *httptest.ResponseRecorder {
	return cutoverFinalReqAs(t, wid, migID, key, body, "test-admin", "")
}

func cutoverFinalReqAs(t *testing.T, wid, migID, key, body, actor, actorType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+migID+"/cutover/final", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	withBearer(req, actor, actorType)
	rec := httptest.NewRecorder()
	postCutoverFinal(rec, req, wid, migID)
	return rec
}

func decodeCutover(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// cutoverFixture builds a migration ready for transfer: evidence chain,
// canary walk, fakes (CDC lag 5, source+target quiesce fakes), and a decided
// approval bound to the measured runtime.
func cutoverFixture(t *testing.T, key string) (wid, migID, apprID string, cf *fakeRehearsalDataPlane, qsrc, qtgt *fakeQuiesceClient, restore func()) {
	t.Helper()
	resetStore()
	wid = registerPlanWID(t, key+"reg00001")
	evalCompat(t, wid, key+"cmp00001")
	evalPlanForDrift(t, wid)
	evalDriftForPlan(t, wid, "clean")
	migID = createMig(t, wid)
	canaryWalk(t, wid, migID, key+"can0001")
	cf = okFake()
	qsrc = &fakeQuiesceClient{quiesced: true, ownership: "aws"}
	qtgt = &fakeQuiesceClient{quiesced: false, ownership: "aws"}
	r1 := useFake(cf)
	r2 := useQuiesceFakes(qsrc, qtgt)
	restore = func() { r2(); r1() }
	runtime := `{"target_weight":0,"environment":"dev","validation_status":"passed","cdc_lag_seconds":5,"target_healthy":true,"read_only_canary":true,"write_ownership":"aws"}`
	arec := postApprovalReq(t, wid, key+"apr00001", runtime, "requester-1", "human")
	if arec.Code != 201 {
		t.Fatalf("approval: %d %s", arec.Code, arec.Body.String())
	}
	appr := decodeApproval(t, arec)
	if drec := decideReq(t, wid, appr.ID, key+"dec00001", "approved", "approver-1", "human"); drec.Code != 200 {
		t.Fatalf("decide: %d", drec.Code)
	}
	return wid, migID, appr.ID, cf, qsrc, qtgt, restore
}

func cutoverBody(apprID string) string {
	return `{"environment":"dev","approval_id":"` + apprID + `"}`
}

// Happy path: COMPLETE with exactly one transfer and full transcript.
func TestCutoverComplete(t *testing.T) {
	wid, migID, apprID, _, qsrc, qtgt, restore := cutoverFixture(t, "cutoverhappy0001")
	defer restore()
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverhappykey01", cutoverBody(apprID)))
	if out["status"] != CutoverComplete || out["stage"] != CutoverComplete {
		t.Fatalf("status=%v stage=%v failure=%v %v", out["status"], out["stage"], out["failure_code"], out["failure_reason"])
	}
	if out["previous_owner"] != "aws" || out["current_owner"] != "azure" || out["routing"] != "azure" {
		t.Fatalf("ownership: %+v", out)
	}
	// Measured positions propagate to the top level (struct/map agnostic).
	if out["source_lsn"] != "0/A001" || out["applied_lsn"] != "0/A001" {
		t.Fatalf("positions: %+v", out)
	}
	if out["cdc_lag_seconds"] != float64(5) {
		t.Fatalf("lag: %+v", out)
	}
	if out["approval_id"] != apprID || out["policy_decision"] == nil {
		t.Fatalf("policy: %+v", out)
	}
	stages, _ := out["stages"].([]any)
	want := []string{"FINAL_PREFLIGHT", "WRITES_QUIESCED", "CDC_CATCHING_UP", "CDC_CAUGHT_UP",
		"FINAL_VALIDATION", "OWNERSHIP_TRANSFERRED", "TRAFFIC_SWITCHED", "WRITES_RESUMED", "CUTOVER_COMPLETE"}
	if len(stages) != len(want) {
		t.Fatalf("stages=%v", stages)
	}
	for i, s := range stages {
		if m, _ := s.(map[string]any); m["name"] != want[i] {
			t.Fatalf("stage %d = %v, want %s", i, m["name"], want[i])
		}
	}
	rec, ok := store.GetOwnership(migID)
	if !ok || rec.CurrentOwner != "azure" || rec.PreviousOwner != "aws" || rec.Routing != "azure" {
		t.Fatalf("ownership record: %+v %v", rec, ok)
	}
	if rec.ApprovalID != apprID {
		t.Fatalf("approval binding: %+v", rec)
	}
	// Split-brain check: source rejects (azure-named), target accepts naming.
	if qsrc.ownership != "azure" || qtgt.ownership != "azure" {
		t.Fatalf("admin states src=%s tgt=%s", qsrc.ownership, qtgt.ownership)
	}
	if !qtgt.quiesced == false {
		t.Fatal("target must be unquiesced")
	}
	// No executions, lifecycle untouched.
	if len(store.GetExecutionsForMigration(migID)) != 0 {
		t.Fatal("executions created")
	}
	if mig, _ := store.GetMigration(migID); mig["status"] != "REGISTERED" {
		t.Fatalf("migration mutated: %+v", mig)
	}
	// Audit chain present.
	ms := store.(*MemStore)
	seen := map[string]bool{}
	for _, a := range ms.audits {
		seen[a.Action] = true
	}
	for _, want := range []string{"final_preflight", "transfer_ownership", "switch_routing", "resume_writes", "cutover_complete", "final_cutover"} {
		if !seen[want] {
			t.Fatalf("missing audit %q", want)
		}
	}
}

// A/B/C/D: stale evidence blocks preflight.
func TestCutoverStaleEvidence(t *testing.T) {
	cases := []struct {
		name  string
		mutate func(t *testing.T, wid string)
	}{
		{"stale-planner", func(t *testing.T, wid string) {
			plan, _ := store.GetLatestTargetPlan(wid)
			plan.PlannerVersion = "planner-v1"
			_ = store.SaveTargetPlan(plan)
		}},
		{"stale-plan", func(t *testing.T, wid string) {
			plan, _ := store.GetLatestTargetPlan(wid)
			plan.CompatibilityReportID = "compat-bogus"
			_ = store.SaveTargetPlan(plan)
		}},
		{"stale-compat", func(t *testing.T, wid string) {
			_ = store.SaveCompatReport(CompatReport{ID: "compat-bogus", WorkloadID: wid,
				TargetProvider: "azure", Status: "pass", RegistryVersion: 1})
		}},
		{"stale-drift", func(t *testing.T, wid string) {
			plan, _ := store.GetLatestTargetPlan(wid)
			_ = store.SaveDriftReport(DriftReport{ID: "drift-bogus", WorkloadID: wid,
				Status: "open", DesiredReference: plan.ID, ObservedReference: "x",
				DriftGate: "blocking",
				Findings: []DriftFinding{{FindingID: "f", Severity: "blocking"}}})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wid, migID, apprID, _, _, _, restore := cutoverFixture(t, "cutoverstale"+c.name)
			defer restore()
			c.mutate(t, wid)
			out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverstalekey01", cutoverBody(apprID)))
			if out["status"] != CutoverBlocked || out["stage"] != "FINAL_PREFLIGHT" {
				t.Fatalf("status=%v stage=%v", out["status"], out["stage"])
			}
			if _, ok := store.GetOwnership(migID); ok {
				t.Fatal("blocked cutover must not record ownership")
			}
		})
	}
}

// E: blocking drift blocks preflight.
func TestCutoverBlockingDrift(t *testing.T) {
	wid, migID, apprID, _, _, _, restore := cutoverFixture(t, "cutoverdrift0001")
	defer restore()
	evalDriftForPlan(t, wid, "versioning-off")
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverdriftkey01", cutoverBody(apprID)))
	if out["status"] != CutoverBlocked {
		t.Fatalf("status=%v", out["status"])
	}
}

// F/G: missing and stale approval block.
func TestCutoverApprovalGates(t *testing.T) {
	wid, migID, _, _, _, _, restore := cutoverFixture(t, "cutoverappr00001")
	defer restore()
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverapprkey01", `{"environment":"dev"}`))
	if out["status"] != CutoverBlocked {
		t.Fatalf("missing approval: %v", out["status"])
	}
	out2 := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverapprkey02", `{"environment":"dev","approval_id":"nope"}`))
	if out2["status"] != CutoverBlocked {
		t.Fatalf("bogus approval: %v", out2["status"])
	}
}

// H: policy deny (unknown compat) blocks.
func TestCutoverPolicyDeny(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "cutoverdeny000001")
	wl, _ := store.GetWorkload(wid)
	spec, _ := wl["canonical_spec"].(map[string]any)
	specRaw, _ := json.Marshal(spec)
	specHash, _ := canonicalHash(specRaw)
	reg, _ := EmbeddedRegistry()
	compatID := ReportID(wid, reg.Version, specHash)
	_ = store.SaveCompatReport(CompatReport{ID: compatID, WorkloadID: wid,
		TargetProvider: "azure", Status: "unknown", RegistryVersion: reg.Version})
	migID := createMig(t, wid)
	defer useFake(okFake())()
	defer useQuiesceFakes(&fakeQuiesceClient{ownership: "aws"}, &fakeQuiesceClient{ownership: "aws"})()
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverdenykey001", `{"environment":"dev"}`))
	if out["status"] != CutoverBlocked {
		t.Fatalf("status=%v", out["status"])
	}
}

// I: canary not walked blocks readiness inside preflight.
func TestCutoverCanaryMissing(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "cutovercan0000001")
	evalCompat(t, wid, "cutovercancmp0001")
	evalPlanForDrift(t, wid)
	evalDriftForPlan(t, wid, "clean")
	migID := createMig(t, wid)
	defer useFake(okFake())()
	defer useQuiesceFakes(&fakeQuiesceClient{quiesced: true, ownership: "aws"}, &fakeQuiesceClient{ownership: "aws"})()
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutovercankey0001", `{"environment":"dev"}`))
	if out["status"] != CutoverBlocked {
		t.Fatalf("status=%v (canary missing must block)", out["status"])
	}
}

// craftPassForCutover stores pass evidence directly.
func craftPassForCutover(t *testing.T, wid string) {
	t.Helper()
	wl, _ := store.GetWorkload(wid)
	spec, _ := wl["canonical_spec"].(map[string]any)
	specRaw, _ := json.Marshal(spec)
	specHash, _ := canonicalHash(specRaw)
	reg, _ := EmbeddedRegistry()
	compatID := ReportID(wid, reg.Version, specHash)
	_ = store.SaveCompatReport(CompatReport{ID: compatID, WorkloadID: wid,
		TargetProvider: "azure", Status: "pass", RegistryVersion: reg.Version})
	_ = store.SaveTargetPlan(TargetMigrationPlan{ID: "plan-cut-pass", WorkloadID: wid,
		SourceProvider: "aws", TargetProvider: "azure", CanonicalSpecVersion: 1,
		CompatibilityReportID: compatID, CompatibilityRegistryVersion: reg.Version,
		PlannerVersion: PlannerVersion, OverallStatus: "pass"})
	_ = store.SaveDriftReport(DriftReport{ID: "drift-cut-pass", WorkloadID: wid,
		Status: "open", DesiredReference: "plan-cut-pass", ObservedReference: "s",
		DriftGate: "clear", Findings: []DriftFinding{}})
}

// J: writes not quiesced blocks preflight at the quiesce gate.
func TestCutoverQuiesceGate(t *testing.T) {
	wid, migID, apprID, _, qsrc, _, restore := cutoverFixture(t, "cutoverquiesce001")
	defer restore()
	qsrc.quiesced = false // prior quiesce undone; transfer must refuse
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverquiescekey", cutoverBody(apprID)))
	if out["status"] != CutoverBlocked || out["stage"] != "FINAL_PREFLIGHT" {
		t.Fatalf("status=%v stage=%v", out["status"], out["stage"])
	}
	if _, ok := store.GetOwnership(migID); ok {
		t.Fatal("unquiesced cutover must not transfer")
	}
}

// K: lag breach blocks preflight.
func TestCutoverLagBreach(t *testing.T) {
	wid, migID, apprID, cf, _, _, restore := cutoverFixture(t, "cutoverlag000001")
	defer restore()
	cf.Report = healthyCDCReport("0/F001", "0/F001")
	cf.Report.LagSeconds = 61
	cf.Report.ObserveUnix = cf.Report.SourceCommitUnix + 61
	cf.Report.WithinRPO = false
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverlagkey001", cutoverBody(apprID)))
	if out["status"] != CutoverBlocked || out["stage"] != "FINAL_PREFLIGHT" {
		t.Fatalf("status=%v stage=%v", out["status"], out["stage"])
	}
	if _, ok := store.GetOwnership(migID); ok {
		t.Fatal("lag breach must not transfer")
	}
}

// L: reconciliation mismatch blocks preflight.
func TestCutoverReconMismatch(t *testing.T) {
	wid, migID, apprID, cf, _, _, restore := cutoverFixture(t, "cutoverrecon00001")
	defer restore()
	tables := cleanRecon(2).Tables
	tables[0].MissingIDs = []string{"probe-1:target"}
	cf.Recon = Reconciliation{Tables: tables, Match: false, Fingerprint: reconFingerprint(tables)}
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverreconkey01", cutoverBody(apprID)))
	if out["status"] != CutoverBlocked {
		t.Fatalf("status=%v", out["status"])
	}
}

// M: concurrent cutovers collapse to a single transfer.
func TestCutoverConcurrency(t *testing.T) {
	wid, migID, apprID, _, qsrc, qtgt, restore := cutoverFixture(t, "cutoverconc000001")
	defer restore()
	const n = 8
	out := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = cutoverFinalReq(t, wid, migID, "cutoverconckey001", cutoverBody(apprID)).Body.String()
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if out[i] != out[0] {
			t.Fatal("concurrent cutovers diverged")
		}
	}
	var res map[string]any
	_ = json.Unmarshal([]byte(out[0]), &res)
	if res["status"] != CutoverComplete {
		t.Fatalf("status=%v", res["status"])
	}
	if qsrc.ownershipCalls != 1 || qtgt.ownershipCalls != 1 {
		t.Fatalf("transfer calls src=%d tgt=%d (want 1/1)", qsrc.ownershipCalls, qtgt.ownershipCalls)
	}
}

// N/V: replay of the completed cutover is byte-identical; no second transfer.
func TestCutoverReplay(t *testing.T) {
	wid, migID, apprID, _, qsrc, qtgt, restore := cutoverFixture(t, "cutoverreplay00001")
	defer restore()
	r1 := cutoverFinalReq(t, wid, migID, "cutoverreplaykey1", cutoverBody(apprID))
	r2 := cutoverFinalReq(t, wid, migID, "cutoverreplaykey1", cutoverBody(apprID))
	if r1.Code != 200 || r2.Code != 200 || r1.Body.String() != r2.Body.String() {
		t.Fatal("replay broken")
	}
	if qsrc.ownershipCalls != 1 || qtgt.ownershipCalls != 1 {
		t.Fatalf("replay re-transferred: src=%d tgt=%d", qsrc.ownershipCalls, qtgt.ownershipCalls)
	}
}

// W: same key + different body conflicts.
func TestCutoverConflict(t *testing.T) {
	wid, migID, apprID, _, _, _, restore := cutoverFixture(t, "cutoverconf000001")
	defer restore()
	r1 := cutoverFinalReq(t, wid, migID, "cutoverconfkey001", cutoverBody(apprID))
	if r1.Code != 200 {
		t.Fatalf("setup: %d", r1.Code)
	}
	r2 := cutoverFinalReq(t, wid, migID, "cutoverconfkey001", `{"environment":"staging","approval_id":"`+apprID+`"}`)
	if r2.Code != 409 {
		t.Fatalf("conflict: %d", r2.Code)
	}
}

// X: stale worker (new key after completion) is refused, transfer intact.
func TestCutoverStaleWorker(t *testing.T) {
	wid, migID, apprID, _, qsrc, qtgt, restore := cutoverFixture(t, "cutoverstale00001")
	defer restore()
	if rec := cutoverFinalReq(t, wid, migID, "cutoverstalekey01", cutoverBody(apprID)); rec.Code != 200 {
		t.Fatalf("setup: %d", rec.Code)
	}
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverstalekey02", cutoverBody(apprID)))
	if out["status"] != CutoverBlocked || out["failure_code"] != CutoverAlreadyTransferred {
		t.Fatalf("stale: status=%v failure=%v", out["status"], out["failure_code"])
	}
	if qsrc.ownershipCalls != 1 || qtgt.ownershipCalls != 1 {
		t.Fatalf("stale worker re-transferred: %d/%d", qsrc.ownershipCalls, qtgt.ownershipCalls)
	}
	if rec, _ := store.GetOwnership(migID); rec.CurrentOwner != "azure" {
		t.Fatalf("record: %+v", rec)
	}
}

// O: target flip failure stays paused-safe; the committed fact stands and
// retry resumes to COMPLETE (H-3 commit-before-expose: the fact commits
// before flips, so a flip failure no longer loses the transfer).
func TestCutoverTargetFlipFailure(t *testing.T) {
	wid, migID, apprID, _, qsrc, qtgt, restore := cutoverFixture(t, "cutoverfail000001")
	defer restore()
	qtgt.failOwnership = errFake("target admin unreachable")
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverfailkey001", cutoverBody(apprID)))
	if out["status"] != CutoverBlocked || out["failure_code"] != CutoverTargetFlipFailed {
		t.Fatalf("status=%v failure=%v", out["status"], out["failure_code"])
	}
	// Paused-safe: source rejects (azure), target still rejects (aws-named).
	if qsrc.ownership != "azure" || qtgt.ownership != "aws" {
		t.Fatalf("unsafe intermediate src=%s tgt=%s", qsrc.ownership, qtgt.ownership)
	}
	// H-3: the ownership fact was committed before the flips, so the failed
	// attempt leaves the authoritative azure record for resume (exactly once).
	if rec, ok := store.GetOwnership(migID); !ok || rec.CurrentOwner != "azure" {
		t.Fatalf("committed fact must stand: %+v %v", rec, ok)
	}
	// Retry resumes through the partial state to COMPLETE.
	out2 := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverfailkey002", cutoverBody(apprID)))
	if out2["status"] != CutoverComplete {
		t.Fatalf("resume: status=%v failure=%v %v", out2["status"], out2["failure_code"], out2["failure_reason"])
	}
}

// P: target resume failure blocks at WRITES_RESUMED without corrupting ownership.
func TestCutoverResumeFailure(t *testing.T) {
	wid, migID, apprID, _, _, qtgt, restore := cutoverFixture(t, "cutoverresume00001")
	defer restore()
	qtgt.failUnquiesce = errFake("target resume refused")
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverresumekey01", cutoverBody(apprID)))
	if out["status"] != CutoverBlocked || out["failure_code"] != CutoverResumeFailed {
		t.Fatalf("status=%v failure=%v", out["status"], out["failure_code"])
	}
	if rec, ok := store.GetOwnership(migID); !ok || rec.CurrentOwner != "azure" {
		t.Fatalf("ownership must stand: %+v %v", rec, ok)
	}
}

// T: post-transfer traffic-only rollback to AWS is rejected.
func TestCutoverRollbackBlocked(t *testing.T) {
	wid, migID, apprID, _, _, _, restore := cutoverFixture(t, "cutoverrollb00001")
	defer restore()
	if rec := cutoverFinalReq(t, wid, migID, "cutoverrollbkey01", cutoverBody(apprID)); rec.Code != 200 {
		t.Fatalf("setup: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+migID+"/rollback", strings.NewReader(`{}`))
	withBearer(req, "test-admin", "")
	rec := httptest.NewRecorder()
	rollback(rec, req)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "POST_WRITE_ROLLBACK_BLOCKED") {
		t.Fatalf("rollback: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reverse CDC") {
		t.Fatalf("must name reverse CDC: %s", rec.Body.String())
	}
	if rec, _ := store.GetOwnership(migID); rec.CurrentOwner != "azure" {
		t.Fatalf("ownership reversed: %+v", rec)
	}
	_ = wid
}

// U: pre-transfer rollback remains allowed (AWS authoritative).
func TestCutoverPreTransferRollback(t *testing.T) {
	_, migID, _, _, _, _, restore := cutoverFixture(t, "cutoverrollu00001")
	defer restore()
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+migID+"/rollback", strings.NewReader(`{}`))
	withBearer(req, "test-admin", "")
	rec := httptest.NewRecorder()
	rollback(rec, req)
	if rec.Code != 202 {
		t.Fatalf("pre-transfer rollback: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := store.GetOwnership(migID); ok {
		t.Fatal("rollback must not create ownership")
	}
}
