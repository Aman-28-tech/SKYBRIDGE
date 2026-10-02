// Policy-gate wiring tests: decisions from stored evidence (not stubs).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func evalPlanForDrift(t *testing.T, wid string) {
	evalPlanForDriftAs(t, wid, "test-admin")
}

func evalPlanForDriftAs(t *testing.T, wid, actor string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads/"+wid+"/plan", strings.NewReader(`{"target_provider":"azure"}`))
	req.Header.Set("Idempotency-Key", "planhelperkey000"+wid[:4])
	withBearer(req, actor, "")
	rec := httptest.NewRecorder()
	postPlan(rec, req, wid)
	if rec.Code != 202 {
		t.Fatalf("setup plan: %d %s", rec.Code, rec.Body.String())
	}
}

// evalDriftForPlan saves a drift evaluation for the latest plan (clean by default).
func evalDriftForPlan(t *testing.T, wid, fixture string) {
	t.Helper()
	ms, ok := store.(*MemStore)
	if !ok {
		t.Fatal("expected MemStore")
	}
	plan, ok := ms.GetLatestTargetPlan(wid)
	if !ok {
		t.Fatal("no plan to drift against")
	}
	if err := store.SaveDriftReport(EvaluateDrift(plan, loadFixture(t, fixture))); err != nil {
		t.Fatal(err)
	}
}

// wiredPassSetup builds a full evidence chain ending in allow:
// register -> hand-inserted PASS compat (deterministic ID) -> plan ->
// clear drift -> migration. Returns workload and migration IDs.
func wiredPassSetup(t *testing.T) (string, string) {
	t.Helper()
	_, body := register(t, "wiredpasskey00001")
	var wl map[string]any
	_ = json.Unmarshal([]byte(body), &wl)
	wid := wl["id"].(string)
	spec := wl["canonical_spec"].(map[string]any)
	raw, _ := json.Marshal(spec)
	specHash, _ := canonicalHash(raw)
	// Real pass evaluation (full-evidence registry) so check requirements
	// match what the planner consumes; stored under the deterministic ID.
	evalRep := Evaluate(spec, fullPassRegistry(t), "azure")
	if evalRep.Status != "pass" {
		t.Fatalf("setup evaluation: %s", evalRep.Status)
	}
	rep := CompatReport{
		ID: ReportID(wid, 1, specHash), WorkloadID: wid, TargetProvider: "azure",
		Status: "pass", RegistryVersion: 1, EvaluatorVersion: EvaluatorVersion,
		Checks: evalRep.Checks,
	}
	if err := store.SaveCompatReport(rep); err != nil {
		t.Fatal(err)
	}
	plan := BuildPlan(spec, rep, testRegistry(t))
	if err := store.SaveTargetPlan(plan); err != nil {
		t.Fatal(err)
	}
	rep2 := EvaluateDrift(plan, loadFixture(t, "clean"))
	if rep2.DriftGate != "clear" {
		t.Fatalf("setup drift: %+v", rep2.Findings)
	}
	if err := store.SaveDriftReport(rep2); err != nil {
		t.Fatal(err)
	}
	return wid, createMig(t, wid)
}

func policyInput(t *testing.T, wid string, runtime map[string]any) PolicyInput {
	t.Helper()
	in, fail := buildPolicyInput(wid, runtime, Principal{ID: "operator", Type: "human"})
	if fail != nil {
		t.Fatalf("buildPolicyInput: %v", fail)
	}
	return in
}

// Allow from wired stored evidence (nothing stubbed).
func TestPolicyAllowWired(t *testing.T) {
	resetStore()
	wid, _ := wiredPassSetup(t)
	in := policyInput(t, wid, map[string]any{"target_weight": 1})
	if in.CompatibilityStatus != "pass" || in.BlockingDriftCount != 0 || in.RPOSeconds != 30 {
		t.Fatalf("inputs not from evidence: %+v", in)
	}
	dec, reasons := policyDecide(in)
	if dec != PolicyAllow || len(reasons) != 0 {
		t.Fatalf("dec=%s reasons=%v", dec, reasons)
	}
	if h1, h2 := in.inputHash(), policyInput(t, wid, map[string]any{"target_weight": 1}).inputHash(); h1 != h2 {
		t.Fatal("input hash nondeterministic")
	}
	if h3 := policyInput(t, wid, map[string]any{"target_weight": 5}).inputHash(); h3 == in.inputHash() {
		t.Fatal("input hash insensitive to inputs")
	}
}

// Conditional / unknown / block from stored reports.
func TestPolicyCompatVariants(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "policyvarkey00001")
	evalCompat(t, wid, "policyvarkey00002") // conditional via embedded registry
	evalPlanForDrift(t, wid)
	evalDriftForPlan(t, wid, "clean")
	// conditional -> approval_required
	in := policyInput(t, wid, map[string]any{"target_weight": 1})
	if dec, _ := policyDecide(in); dec != PolicyApprovalRequired {
		t.Fatalf("conditional: %s", dec)
	}
	// unknown / block via hand-set reports (deterministic IDs preserved)
	for _, status := range []string{"unknown", "block"} {
		wl, _ := store.GetWorkload(wid)
		spec := wl["canonical_spec"].(map[string]any)
		raw, _ := json.Marshal(spec)
		h, _ := canonicalHash(raw)
		rep := CompatReport{ID: ReportID(wid, 1, h), WorkloadID: wid, TargetProvider: "azure",
			Status: status, RegistryVersion: 1, EvaluatorVersion: EvaluatorVersion,
			Checks: []Check{{Requirement: "x", TargetCapability: "y", Status: status}}}
		_ = store.SaveCompatReport(rep)
		plan := BuildPlan(spec, rep, testRegistry(t))
		_ = store.SaveTargetPlan(plan)
		_ = store.SaveDriftReport(EvaluateDrift(plan, loadFixture(t, "clean")))
		// NOTE: clean snapshot vs non-pass plan still evaluates structurally;
		// decision must deny regardless.
		dec, reasons := policyDecide(policyInput(t, wid, map[string]any{"target_weight": 1}))
		if dec != PolicyDeny || !strings.Contains(strings.Join(reasons, " "), "compatibility_not_pass") {
			t.Fatalf("%s: %s %v", status, dec, reasons)
		}
	}
}

// Drift severities from stored reports.
func TestPolicyDriftVariants(t *testing.T) {
	newChain := func(t *testing.T, snapName, key string) string {
		t.Helper()
		wid, _ := wiredPassSetup(t)
		ms := store.(*MemStore)
		plan, _ := ms.GetLatestTargetPlan(wid)
		_ = store.SaveDriftReport(EvaluateDrift(plan, loadFixture(t, snapName)))
		_ = key
		return wid
	}
	resetStore()
	wid := newChain(t, "versioning-off", "x")
	if dec, r := policyDecide(policyInput(t, wid, map[string]any{"target_weight": 1})); dec != PolicyDeny || !strings.Contains(strings.Join(r, " "), "blocking_drift") {
		t.Fatalf("blocking: %s %v", dec, r)
	}
	resetStore()
	wid2 := newChain(t, "network-open", "y")
	if dec, r := policyDecide(policyInput(t, wid2, map[string]any{"target_weight": 1})); dec != PolicyDeny || !strings.Contains(strings.Join(r, " "), "security_critical_drift") {
		t.Fatalf("sec: %s %v", dec, r)
	}
	resetStore()
	wid3, _ := wiredPassSetup(t)
	ms := store.(*MemStore)
	plan, _ := ms.GetLatestTargetPlan(wid3)
	_ = store.SaveDriftReport(EvaluateDrift(plan, loadFixture(t, "observability-gap")))
	if dec, _ := policyDecide(policyInput(t, wid3, map[string]any{"target_weight": 1})); dec != PolicyAllow {
		t.Fatalf("informational must not block: %s", dec)
	}
}

// Stale/missing evidence fails explicitly and closed.
func TestPolicyEvidenceFailures(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "policyfailkey001")
	if _, fail := buildPolicyInput(wid, map[string]any{"target_weight": 1}, Principal{ID: "op", Type: "human"}); fail == nil || fail.Code != "COMPATIBILITY_STALE" || fail.Status != 409 {
		t.Fatalf("no-compat: %+v", fail)
	}
	evalCompat(t, wid, "policyfailkey002")
	if _, fail := buildPolicyInput(wid, map[string]any{"target_weight": 1}, Principal{ID: "op", Type: "human"}); fail == nil || fail.Status != 404 {
		t.Fatalf("no-plan: %+v", fail)
	}
	evalPlanForDrift(t, wid)
	if _, fail := buildPolicyInput(wid, map[string]any{"target_weight": 1}, Principal{ID: "op", Type: "human"}); fail == nil || fail.Code != "NO_DRIFT_EVIDENCE" || fail.Status != 409 {
		t.Fatalf("no-drift: %+v", fail)
	}
	// stale planner version
	ms := store.(*MemStore)
	plan, _ := ms.GetLatestTargetPlan(wid)
	plan.PlannerVersion = "planner-v0"
	_ = ms.SaveTargetPlan(plan)
	_ = store.SaveDriftReport(EvaluateDrift(plan, loadFixture(t, "clean")))
	if _, fail := buildPolicyInput(wid, map[string]any{"target_weight": 1}, Principal{ID: "op", Type: "human"}); fail == nil || fail.Code != "PLAN_STALE" {
		t.Fatalf("stale-plan: %+v", fail)
	}
}

// Fail-closed: unknown actions, absent weight, and garbage can only deny.
func TestPolicyFailClosed(t *testing.T) {
	in := basePolicy()
	in.Action = "migrate_database"
	if dec, _ := policyDecide(in); dec != PolicyDeny {
		t.Fatalf("unknown action: %s", dec)
	}
	empty := PolicyInput{}
	if dec, reasons := policyDecide(empty); dec != PolicyDeny || len(reasons) == 0 {
		t.Fatalf("empty input: %s %v", dec, reasons)
	}
	// totality over hostile inputs: never allow, never empty decision
	battery := []PolicyInput{
		{},
		{Action: "shift_traffic", Environment: "dev", TargetWeight: -1},
		{Action: "shift_traffic", Environment: "dev", TargetWeight: 1, CompatibilityStatus: "pass", ValidationStatus: "passed", TargetHealthy: true, ReadOnlyCanary: true, WriteOwnership: "azure", RPOSeconds: 30, CDCLagSeconds: 0},
		{Action: "", Environment: "", TargetWeight: 100},
	}
	for i, b := range battery {
		if dec, reasons := policyDecide(b); dec == "" || dec == PolicyAllow && len(reasons) != 0 {
			t.Fatalf("battery %d: %s %v", i, dec, reasons)
		} else if dec == PolicyAllow {
			t.Fatalf("battery %d unexpectedly allowed", i)
		}
	}
}

// Endpoint: allow + gates + audit + idempotency + race.
func postReadinessReq(t *testing.T, wid, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads/"+wid+"/readiness", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	withBearer(req, "test-admin", "")
	rec := httptest.NewRecorder()
	postReadiness(rec, req, wid)
	return rec
}

func TestReadinessEndpoint(t *testing.T) {
	resetStore()
	wid, _ := wiredPassSetup(t)
	ms := store.(*MemStore)
	auditsBefore := len(ms.audits)
	rec := postReadinessReq(t, wid, "readinesskey00001", `{"target_weight":1}`)
	if rec.Code != 200 {
		t.Fatalf("readiness: %d %s", rec.Code, rec.Body.String())
	}
	var dec map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &dec); err != nil {
		t.Fatal(err)
	}
	if dec["decision"] != PolicyAllow {
		t.Fatalf("decision=%v", dec["decision"])
	}
	gates, _ := dec["gates"].([]any)
	if len(gates) != 9 {
		t.Fatalf("gates=%d, want 9", len(gates))
	}
	if dec["policy_bundle_version"] == "" || dec["policy_input_hash"] == "" {
		t.Fatalf("missing version/hash: %v", dec)
	}
	if len(ms.audits) != auditsBefore+1 { // audit persisted with hash
		t.Fatalf("audits=%d", len(ms.audits))
	}
	last := ms.audits[len(ms.audits)-1]
	if last.PolicyDecision != PolicyAllow || last.Result != "success" ||
		!strings.Contains(last.Metadata, dec["policy_input_hash"].(string)) {
		t.Fatalf("audit: %+v", last)
	}
	// replay + conflict
	rec2 := postReadinessReq(t, wid, "readinesskey00001", `{"target_weight":1}`)
	if rec2.Code != 200 || rec2.Body.String() != rec.Body.String() {
		t.Fatal("replay broken")
	}
	if len(ms.audits) != auditsBefore+1 {
		t.Fatal("replay audited")
	}
	rec3 := postReadinessReq(t, wid, "readinesskey00001", `{"target_weight":5}`)
	if rec3.Code != 409 {
		t.Fatalf("expected 409, got %d", rec3.Code)
	}
	// failures: unknown workload, missing weight, stale evidence
	if rec := postReadinessReq(t, "00000000-0000-0000-0000-000000000000", "readinesskey00002", `{"target_weight":1}`); rec.Code != 404 {
		t.Fatalf("unknown workload: %d", rec.Code)
	}
	if rec := postReadinessReq(t, wid, "readinesskey00003", `{"environment":"dev"}`); rec.Code != 400 {
		t.Fatalf("missing weight: %d", rec.Code)
	}
}

// Conditional via real chain -> approval_required with gates; denial on drift.
func TestReadinessConditionalAndDeny(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "readinesscond001")
	evalCompat(t, wid, "readinesscond002")
	evalPlanForDrift(t, wid)
	ms := store.(*MemStore)
	plan, _ := ms.GetLatestTargetPlan(wid)
	_ = store.SaveDriftReport(EvaluateDrift(plan, loadFixture(t, "clean")))
	rec := postReadinessReq(t, wid, "readinesskey00004", `{"target_weight":1}`)
	var dec map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &dec)
	if rec.Code != 200 || dec["decision"] != PolicyApprovalRequired {
		t.Fatalf("conditional: %d %v", rec.Code, dec["decision"])
	}
	_ = store.SaveDriftReport(EvaluateDrift(plan, loadFixture(t, "versioning-off")))
	rec2 := postReadinessReq(t, wid, "readinesskey00005", `{"target_weight":1}`)
	var dec2 map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &dec2)
	if rec2.Code != 200 || dec2["decision"] != PolicyDeny {
		t.Fatalf("blocking drift: %d %v", rec2.Code, dec2["decision"])
	}
}

// Race: concurrent identical readiness -> identical bodies, single audit.
func TestReadinessRace(t *testing.T) {
	resetStore()
	wid, _ := wiredPassSetup(t)
	ms := store.(*MemStore)
	auditsBefore := len(ms.audits)
	const n = 16
	codes := make([]int, n)
	bodies := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := postReadinessReq(t, wid, "readinessracekey0", `{"target_weight":1}`)
			codes[i], bodies[i] = rec.Code, rec.Body.String()
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if codes[i] != 200 || bodies[i] != bodies[0] {
			t.Fatalf("goroutine %d: %d", i, codes[i])
		}
	}
	if len(ms.audits) != auditsBefore+1 {
		t.Fatalf("audits=%d, want single evaluation audit", len(ms.audits)-auditsBefore)
	}
}
