// Readiness gate tests: READY_FOR_CUTOVER without ownership transfer
// (run: go test ./...).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func readinessReq(t *testing.T, wid, migID, key, body string) *httptest.ResponseRecorder {
	return readinessReqAs(t, wid, migID, key, body, "test-admin")
}

func readinessReqAs(t *testing.T, wid, migID, key, body, actor string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+migID+"/readiness", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	withBearer(req, actor, "")
	rec := httptest.NewRecorder()
	postReadinessForCutover(rec, req, wid, migID)
	return rec
}

func decodeReadiness(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
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

func checkByName(t *testing.T, out map[string]any, name string) map[string]any {
	t.Helper()
	checks, _ := out["checks"].([]any)
	for _, c := range checks {
		m, _ := c.(map[string]any)
		if m["name"] == name {
			return m
		}
	}
	t.Fatalf("check %q missing in %+v", name, out["checks"])
	return nil
}

// readinessChain builds evidence + canary walk; caller sets quiesce/CDC fakes.
func readinessChain(t *testing.T, key string) (wid, migID string) {
	t.Helper()
	resetStore()
	wid = registerPlanWID(t, key+"reg00001")
	evalCompat(t, wid, key+"cmp00001")
	evalPlanForDrift(t, wid)
	evalDriftForPlan(t, wid, "clean")
	migID = createMig(t, wid)
	canaryWalk(t, wid, migID, key+"can0001")
	return wid, migID
}

const readinessBody = `{"environment":"dev"}`

// Happy path on pass evidence: READY_FOR_CUTOVER, ownership untouched.
func TestReadinessHappyPass(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "readyhappy000001")
	planID, compatID, driftID := craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	canaryWalk(t, wid, migID, "readyhcan00001")
	qf := &fakeQuiesceClient{quiesced: true, ownership: "aws"}
	defer useQuiesceFake(qf)()
	defer useFake(okFake())()
	out := decodeReadiness(t, readinessReq(t, wid, migID, "readyhkey0000001", readinessBody))
	if out["status"] != ReadyForCutover {
		t.Fatalf("status=%v: %s", out["status"], readinessReq(t, wid, migID, "readyhkey0000002", readinessBody).Body.String())
	}
	for _, name := range []string{"evidence_fresh", "canary", "quiesce", "ownership", "cdc_catchup", "reconciliation", "policy"} {
		if c := checkByName(t, out, name); c["pass"] != true {
			t.Fatalf("check %s failed: %+v", name, c)
		}
	}
	if out["plan_id"] != planID || out["compatibility_report_id"] != compatID || out["drift_report_id"] != driftID {
		t.Fatalf("evidence refs: %+v", out)
	}
	if out["write_ownership"] != "aws" {
		t.Fatalf("ownership: %+v", out)
	}
	if _, hasExec := out["execution_id"]; hasExec {
		t.Fatal("readiness must not start executions")
	}
	if mig, _ := store.GetMigration(migID); mig["status"] != "REGISTERED" {
		t.Fatalf("migration mutated: %+v", mig)
	}
	if len(store.GetExecutionsForMigration(migID)) != 0 {
		t.Fatal("execution rows created")
	}
}

// Conditional path with eligible approval -> READY_FOR_CUTOVER.
func TestReadinessConditionalApproval(t *testing.T) {
	wid, migID := readinessChain(t, "readycond0000001")
	qf := &fakeQuiesceClient{quiesced: true, ownership: "aws"}
	defer useQuiesceFake(qf)()
	defer useFake(okFake())() // lag 5
	runtime := `{"target_weight":0,"environment":"dev","validation_status":"passed","cdc_lag_seconds":5,"target_healthy":true,"read_only_canary":true,"write_ownership":"aws"}`
	arec := postApprovalReq(t, wid, "readycondkey0001", runtime, "requester-1", "human")
	if arec.Code != 201 {
		t.Fatalf("approval: %d %s", arec.Code, arec.Body.String())
	}
	appr := decodeApproval(t, arec)
	if drec := decideReq(t, wid, appr.ID, "readycondkey0002", "approved", "approver-1", "human"); drec.Code != 200 {
		t.Fatalf("decide: %d", drec.Code)
	}
	body := `{"environment":"dev","approval_id":"` + appr.ID + `"}`
	out := decodeReadiness(t, readinessReq(t, wid, migID, "readycondkey0003", body))
	if out["status"] != ReadyForCutover {
		t.Fatalf("status=%v checks=%v", out["status"], out["checks"])
	}
	if out["approval_id"] != appr.ID {
		t.Fatalf("approval: %+v", out)
	}
}

// I: lag above RPO blocks readiness.
func TestReadinessLagBreach(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "readylag00000001")
	craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	canaryWalk(t, wid, migID, "readylcan00001")
	defer useQuiesceFake(&fakeQuiesceClient{quiesced: true, ownership: "aws"})()
	f := okFake()
	f.Report = healthyCDCReport("0/D001", "0/D001")
	f.Report.LagSeconds = 45
	f.Report.ObserveUnix = f.Report.SourceCommitUnix + 45
	f.Report.WithinRPO = false
	defer useFake(f)()
	out := decodeReadiness(t, readinessReq(t, wid, migID, "readylagkey00001", readinessBody))
	if out["status"] != NotReady {
		t.Fatalf("status=%v", out["status"])
	}
	if c := checkByName(t, out, "cdc_catchup"); c["pass"] != false {
		t.Fatalf("cdc check: %+v", c)
	}
}

// Custom RPO: the CDC gate uses the workload-spec RPO resolved through the
// policy input, not a hardcoded 30s. Lag 45 with rpo_seconds=60 is READY
// (policy freshness and cdc_catchup must agree).
func TestReadinessCustomRPO(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "readyrpo00000001")
	wl, _ := store.GetWorkload(wid)
	spec, _ := wl["canonical_spec"].(map[string]any)
	reqs, _ := spec["requirements"].(map[string]any)
	reqs["rpo_seconds"] = float64(60)
	craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	canaryWalk(t, wid, migID, "readyrpocan0001")
	defer useQuiesceFake(&fakeQuiesceClient{quiesced: true, ownership: "aws"})()
	f := okFake()
	f.Report = healthyCDCReport("0/D002", "0/D002")
	f.Report.LagSeconds = 45
	f.Report.ObserveUnix = f.Report.SourceCommitUnix + 45
	f.Report.WithinRPO = true
	defer useFake(f)()
	out := decodeReadiness(t, readinessReq(t, wid, migID, "readyrpokey00001", readinessBody))
	if out["status"] != ReadyForCutover {
		t.Fatalf("status=%v checks=%v", out["status"], out["checks"])
	}
	if c := checkByName(t, out, "cdc_catchup"); c["pass"] != true {
		t.Fatalf("cdc check: %+v", c)
	}
}

// J: CDC not caught up (applied behind base) blocks readiness.
func TestReadinessNotCaughtUp(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "readybehind00001")
	craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	canaryWalk(t, wid, migID, "readybcan00001")
	defer useQuiesceFake(&fakeQuiesceClient{quiesced: true, ownership: "aws"})()
	f := okFake()
	f.Report = healthyCDCReport("0/E010", "0/E005") // applied behind base
	f.Report.BaseLSN = "0/E010"
	defer useFake(f)()
	out := decodeReadiness(t, readinessReq(t, wid, migID, "readybkey0000001", readinessBody))
	if out["status"] != NotReady {
		t.Fatalf("status=%v", out["status"])
	}
	if c := checkByName(t, out, "cdc_catchup"); c["pass"] != false {
		t.Fatalf("cdc check: %+v", c)
	}
}

// K: reconciliation mismatch blocks readiness.
func TestReadinessReconMismatch(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "readyrecon000001")
	craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	canaryWalk(t, wid, migID, "readykcan00001")
	defer useQuiesceFake(&fakeQuiesceClient{quiesced: true, ownership: "aws"})()
	f := okFake()
	tables := cleanRecon(2).Tables
	tables[1].Mismatched = []string{"probe-x"}
	f.Recon = Reconciliation{Tables: tables, Match: false, Fingerprint: reconFingerprint(tables)}
	defer useFake(f)()
	out := decodeReadiness(t, readinessReq(t, wid, migID, "readykkey0000001", readinessBody))
	if out["status"] != NotReady {
		t.Fatalf("status=%v", out["status"])
	}
	if c := checkByName(t, out, "reconciliation"); c["pass"] != false {
		t.Fatalf("recon check: %+v", c)
	}
}

// L/M: stale plan / stale compat invalidate readiness.
func TestReadinessStaleEvidence(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "readystale000001")
	craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	canaryWalk(t, wid, migID, "readyscan00001")
	defer useQuiesceFake(&fakeQuiesceClient{quiesced: true, ownership: "aws"})()
	defer useFake(okFake())()
	first := decodeReadiness(t, readinessReq(t, wid, migID, "readyskey0000001", readinessBody))
	if first["status"] != ReadyForCutover {
		t.Fatalf("setup: %v", first["status"])
	}
	plan, _ := store.GetLatestTargetPlan(wid)
	plan.PlannerVersion = "planner-v1"
	_ = store.SaveTargetPlan(plan)
	second := decodeReadiness(t, readinessReq(t, wid, migID, "readyskey0000002", readinessBody))
	if second["status"] != NotReady {
		t.Fatalf("stale plan must invalidate: %v", second["status"])
	}
	if c := checkByName(t, second, "evidence_fresh"); c["pass"] != false {
		t.Fatalf("evidence check: %+v", c)
	}
}

// N: stale approval on the conditional path blocks readiness.
func TestReadinessStaleApproval(t *testing.T) {
	wid, migID := readinessChain(t, "readystaleap001")
	qf := &fakeQuiesceClient{quiesced: true, ownership: "aws"}
	defer useQuiesceFake(qf)()
	defer useFake(okFake())()
	runtime := `{"target_weight":0,"environment":"dev","validation_status":"passed","cdc_lag_seconds":5,"target_healthy":true,"read_only_canary":true,"write_ownership":"aws"}`
	arec := postApprovalReq(t, wid, "readystaleapkey1", runtime, "requester-1", "human")
	appr := decodeApproval(t, arec)
	if drec := decideReq(t, wid, appr.ID, "readystaleapkey2", "approved", "approver-1", "human"); drec.Code != 200 {
		t.Fatalf("decide: %d", drec.Code)
	}
	evalDriftForPlan(t, wid, "versioning-off") // evidence moves under the approval
	body := `{"environment":"dev","approval_id":"` + appr.ID + `"}`
	out := decodeReadiness(t, readinessReq(t, wid, migID, "readystaleapkey3", body))
	if out["status"] != NotReady {
		t.Fatalf("status=%v (must block)", out["status"])
	}
}

// O: policy deny (unknown compat) blocks readiness.
func TestReadinessPolicyDeny(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "readydeny0000001")
	wl, _ := store.GetWorkload(wid)
	spec, _ := wl["canonical_spec"].(map[string]any)
	specRaw, _ := json.Marshal(spec)
	specHash, _ := canonicalHash(specRaw)
	reg, _ := EmbeddedRegistry()
	compatID := ReportID(wid, reg.Version, specHash)
	_ = store.SaveCompatReport(CompatReport{ID: compatID, WorkloadID: wid,
		TargetProvider: "azure", Status: "unknown",
		RegistryVersion: reg.Version, EvaluatorVersion: EvaluatorVersion})
	migID := createMig(t, wid)
	defer useQuiesceFake(&fakeQuiesceClient{quiesced: true, ownership: "aws"})()
	defer useFake(okFake())()
	out := decodeReadiness(t, readinessReq(t, wid, migID, "readydenykey00001", readinessBody))
	if out["status"] != NotReady {
		t.Fatalf("status=%v", out["status"])
	}
}

// P: readiness invalidates after evidence change (READY then blocking drift).
func TestReadinessInvalidation(t *testing.T) {
	wid, migID := readinessChain(t, "readyinv00000001")
	// conditional path needs approval for READY; use crafted pass instead.
	resetStore()
	wid = registerPlanWID(t, "readyinv00000002")
	craftPassEvidence(t, wid)
	migID = createMig(t, wid)
	canaryWalk(t, wid, migID, "readyinvcan0001")
	defer useQuiesceFake(&fakeQuiesceClient{quiesced: true, ownership: "aws"})()
	defer useFake(okFake())()
	first := decodeReadiness(t, readinessReq(t, wid, migID, "readyinvkey00001", readinessBody))
	if first["status"] != ReadyForCutover {
		t.Fatalf("setup: %v", first["status"])
	}
	plan, _ := store.GetLatestTargetPlan(wid)
	_ = store.SaveDriftReport(DriftReport{ID: "drift-inv-1", WorkloadID: wid,
		Status: "open", DesiredReference: plan.ID, ObservedReference: "snap-2",
		DriftGate: "blocking",
		Findings: []DriftFinding{{FindingID: "f1", Severity: "blocking", Component: "database"}}})
	second := decodeReadiness(t, readinessReq(t, wid, migID, "readyinvkey00002", readinessBody))
	if second["status"] != NotReady {
		t.Fatalf("drift must invalidate readiness: %v", second["status"])
	}
}

// Not quiesced -> blocked; S: resume path (unquiesce then re-quiesce).
func TestReadinessQuiesceGate(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "readyquiesce0001")
	craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	canaryWalk(t, wid, migID, "readyqcan00001")
	qf := &fakeQuiesceClient{quiesced: false, ownership: "aws"}
	defer useQuiesceFake(qf)()
	defer useFake(okFake())()
	out := decodeReadiness(t, readinessReq(t, wid, migID, "readyqkey0000001", readinessBody))
	if out["status"] != NotReady {
		t.Fatalf("unquiesced must block: %v", out["status"])
	}
	if c := checkByName(t, out, "quiesce"); c["pass"] != false {
		t.Fatalf("quiesce check: %+v", c)
	}
	qf.quiesced = true // resume path: re-quiesce after drain
	again := decodeReadiness(t, readinessReq(t, wid, migID, "readyqkey0000002", readinessBody))
	if again["status"] != ReadyForCutover {
		t.Fatalf("re-quiesce must restore readiness: %v", again["status"])
	}
}

// Canary incomplete (only through stage 5) blocks readiness.
func TestReadinessCanaryIncomplete(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "readyinc00000001")
	craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	expected := 0
	for i, s := range []int{0, 1, 5} {
		rec := canaryReq(t, wid, migID, fmt.Sprintf("readyinckey%05d", i), canaryBody(s, expected, goodObs(s)))
		decodeCanary(t, rec)
		expected = canaryNext[s]
	}
	defer useQuiesceFake(&fakeQuiesceClient{quiesced: true, ownership: "aws"})()
	defer useFake(okFake())()
	out := decodeReadiness(t, readinessReq(t, wid, migID, "readyinckey00001", readinessBody))
	if out["status"] != NotReady {
		t.Fatalf("partial canary must block: %v", out["status"])
	}
	if c := checkByName(t, out, "canary"); c["pass"] != false {
		t.Fatalf("canary check: %+v", c)
	}
}

// Readiness idempotency: replay identical, conflict on change.
func TestReadinessIdempotency(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "readyidem0000001")
	craftPassEvidence(t, wid)
	migID := createMig(t, wid)
	canaryWalk(t, wid, migID, "readyidemcan001")
	defer useQuiesceFake(&fakeQuiesceClient{quiesced: true, ownership: "aws"})()
	defer useFake(okFake())()
	r1 := readinessReq(t, wid, migID, "readyidemkey0001", readinessBody)
	r2 := readinessReq(t, wid, migID, "readyidemkey0001", readinessBody)
	if r1.Code != 200 || r2.Code != 200 || r1.Body.String() != r2.Body.String() {
		t.Fatal("replay broken")
	}
	r3 := readinessReq(t, wid, migID, "readyidemkey0001", `{"environment":"staging"}`)
	if r3.Code != 409 {
		t.Fatalf("conflict: %d", r3.Code)
	}
}
