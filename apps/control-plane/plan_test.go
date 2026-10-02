// Target migration plan tests (planning ONLY — no cloud, no workflows).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func planSpec(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(fullSpecBody()), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func fullPassRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := cloneRegistry(t, testRegistry(t))
	c := reg.byID["postgresql.private_networking"]
	c.Evidence = append(c.Evidence, EvidenceRef{Type: "integration_test", Ref: "tests/probe.yaml"})
	return reg
}

func compOf(p TargetMigrationPlan, key string) PlanComponent {
	for _, c := range p.Components {
		if c.Key == key {
			return c
		}
	}
	return PlanComponent{}
}

// A: all-pass compatibility -> complete plan (data-movement stays gated by design).
func TestPlanAllPass(t *testing.T) {
	reg := fullPassRegistry(t)
	spec := planSpec(t)
	rep := Evaluate(spec, reg, "azure")
	if rep.Status != "pass" {
		t.Fatalf("setup: %s", rep.Status)
	}
	plan := BuildPlan(spec, rep, reg)
	if plan.OverallStatus != "pass" {
		t.Fatalf("overall=%s", plan.OverallStatus)
	}
	for _, c := range plan.Components {
		want := ModeAuto
		if c.Key == "data-movement" {
			want = ModeGated
		}
		if c.ProvisioningMode != want {
			t.Fatalf("%s: mode=%s, want %s", c.Key, c.ProvisioningMode, want)
		}
		if c.CompatibilityStatus != "pass" {
			t.Fatalf("%s: status=%s", c.Key, c.CompatibilityStatus)
		}
		if len(c.Evidence) == 0 { // G
			t.Fatalf("%s: no evidence", c.Key)
		}
	}
	if len(plan.Blockers) != 0 || len(plan.ValidationGates) != 0 {
		t.Fatal("clean plan carries gates/blockers")
	}
}

// B: conditional stays conditional with an explicit gate (live shape).
func TestPlanConditionalGate(t *testing.T) {
	reg := testRegistry(t)
	spec := planSpec(t)
	rep := Evaluate(spec, reg, "azure")
	plan := BuildPlan(spec, rep, reg)
	if plan.OverallStatus != "conditional" {
		t.Fatalf("overall=%s", plan.OverallStatus)
	}
	db := compOf(plan, "database")
	if db.CompatibilityStatus != "conditional" || db.ProvisioningMode != ModeGated {
		t.Fatalf("database: status=%s mode=%s", db.CompatibilityStatus, db.ProvisioningMode)
	}
	if db.ValidationGate == "" || !strings.Contains(db.ValidationGate, "conditional") {
		t.Fatalf("database gate missing: %q", db.ValidationGate)
	}
	if len(plan.ValidationGates) == 0 {
		t.Fatal("plan-level gates empty")
	}
}

// C: unknown -> no readiness implied.
func TestPlanUnknown(t *testing.T) {
	reg := cloneRegistry(t, testRegistry(t))
	delete(reg.byID, "routing.weighted_canary")
	kept := reg.Capabilities[:0]
	for _, c := range reg.Capabilities {
		if c.ID != "routing.weighted_canary" {
			kept = append(kept, c)
		}
	}
	reg.Capabilities = kept
	rep := Evaluate(planSpec(t), reg, "azure")
	plan := BuildPlan(planSpec(t), rep, reg)
	if plan.OverallStatus != "unknown" {
		t.Fatalf("overall=%s", plan.OverallStatus)
	}
	rt := compOf(plan, "routing")
	if rt.ProvisioningMode != ModeDescribeOnly || rt.ValidationGate == "" {
		t.Fatalf("routing: mode=%s gate=%q", rt.ProvisioningMode, rt.ValidationGate)
	}
	for _, c := range plan.Components {
		if c.ProvisioningMode == ModeAuto && c.CompatibilityStatus == "unknown" {
			t.Fatalf("%s implies readiness while unknown", c.Key)
		}
	}
}

// D: block -> no executable action for the blocked requirement.
func TestPlanBlocked(t *testing.T) {
	reg := cloneRegistry(t, testRegistry(t))
	reg.byID["queue.at_least_once"].Supported = false
	rep := Evaluate(planSpec(t), reg, "azure")
	plan := BuildPlan(planSpec(t), rep, reg)
	if plan.OverallStatus != "block" {
		t.Fatalf("overall=%s", plan.OverallStatus)
	}
	q := compOf(plan, "queue")
	if q.ProvisioningMode != ModeBlocked || len(q.DesiredConfiguration) != 0 || len(q.Blockers) == 0 {
		t.Fatalf("queue: mode=%s cfg=%v blockers=%v", q.ProvisioningMode, q.DesiredConfiguration, q.Blockers)
	}
	if compOf(plan, "compute").ProvisioningMode != ModeAuto {
		t.Fatal("unblocked component affected")
	}
}

// E: identical inputs -> identical plan. F: stable component ordering.
func TestPlanDeterministicAndOrdered(t *testing.T) {
	reg := testRegistry(t)
	spec := planSpec(t)
	rep := Evaluate(spec, reg, "azure")
	a := BuildPlan(spec, rep, reg)
	b := BuildPlan(spec, rep, reg)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatal("plan nondeterministic")
	}
	if len(a.Components) != len(componentOrder) {
		t.Fatalf("components=%d, want %d", len(a.Components), len(componentOrder))
	}
	for i, key := range componentOrder {
		if a.Components[i].Key != key {
			t.Fatalf("position %d: %s, want %s", i, a.Components[i].Key, key)
		}
	}
}

// Q: planner consumes the report; flipping a verdict flips the plan without
// re-deriving anything from the registry.
func TestPlanDerivedFromReport(t *testing.T) {
	reg := testRegistry(t)
	spec := planSpec(t)
	rep := Evaluate(spec, reg, "azure")
	before := BuildPlan(spec, rep, reg)
	if compOf(before, "routing").ProvisioningMode != ModeAuto {
		t.Fatal("setup: routing not auto")
	}
	for i := range rep.Checks {
		if strings.HasPrefix(rep.Checks[i].Requirement, "routing:") {
			rep.Checks[i].Status = "block"
		}
	}
	rep.Status = Aggregate(rep.Checks)
	after := BuildPlan(spec, rep, reg)
	if after.OverallStatus != "block" {
		t.Fatalf("overall=%s", after.OverallStatus)
	}
	rt := compOf(after, "routing")
	if rt.ProvisioningMode != ModeBlocked || len(rt.DesiredConfiguration) != 0 {
		t.Fatalf("routing: mode=%s cfg=%v", rt.ProvisioningMode, rt.DesiredConfiguration)
	}
}

// Fidelity: object_storage.versioning_required=true must survive as a
// desired-state requirement, distinct from version-ID identity semantics.
func TestPlanVersioningPreserved(t *testing.T) {
	reg := testRegistry(t)
	spec := planSpec(t)
	rep := Evaluate(spec, reg, "azure")
	plan := BuildPlan(spec, rep, reg)
	obj := compOf(plan, "object-storage")
	if obj.DesiredConfiguration["versioning_required"] != true {
		t.Fatalf("versioning_required lost: %v", obj.DesiredConfiguration)
	}
	if obj.DesiredConfiguration["version_id_identity"] != false {
		t.Fatalf("identity semantics lost: %v", obj.DesiredConfiguration)
	}
	if obj.DesiredConfiguration["identity_model"] != "logical-key-plus-sha256" {
		t.Fatalf("identity model lost: %v", obj.DesiredConfiguration)
	}
	found := false
	for _, a := range obj.Assumptions {
		if strings.Contains(a, "Blob versioning must be enabled") {
			found = true
		}
	}
	if !found {
		t.Fatalf("provisioning assumption missing: %v", obj.Assumptions)
	}
}

// Fidelity: merged database capability must be a single canonical identifier.
func TestPlanNoDuplicatedCapability(t *testing.T) {
	reg := testRegistry(t)
	spec := planSpec(t)
	rep := Evaluate(spec, reg, "azure")
	plan := BuildPlan(spec, rep, reg)
	db := compOf(plan, "database")
	if strings.Contains(db.TargetCapability, "+") {
		t.Fatalf("duplicated capability: %q", db.TargetCapability)
	}
	if db.TargetCapability != "azure-database-for-postgresql (azure)" {
		t.Fatalf("capability: %q", db.TargetCapability)
	}
	// Unit: distinct capabilities stay visibly joined.
	a := &Check{Requirement: "r1", TargetCapability: "svc-a (azure)", Status: "pass",
		Evidence: []EvidenceRef{{Type: "provider_documentation", Ref: "x"}}, Assumptions: []string{}}
	b := &Check{Requirement: "r2", TargetCapability: "svc-b (azure)", Status: "pass",
		Evidence: []EvidenceRef{{Type: "provider_documentation", Ref: "y"}}, Assumptions: []string{}}
	m := mergeComponent("k", "req", "svc", map[string]any{}, nil, nil, a, b)
	if m.TargetCapability != "svc-a (azure) + svc-b (azure)" {
		t.Fatalf("joined: %q", m.TargetCapability)
	}
	if len(m.Evidence) != 2 {
		t.Fatal("merged evidence lost a contributing check")
	}
}

// Fidelity: every important canonical requirement survives planning.
// Path syntax: component.key.field (config) or component#field.
func TestPlanRequirementFidelity(t *testing.T) {
	reg := fullPassRegistry(t)
	spec := planSpec(t)
	rep := Evaluate(spec, reg, "azure")
	plan := BuildPlan(spec, rep, reg)
	byKey := map[string]PlanComponent{}
	for _, c := range plan.Components {
		byKey[c.Key] = c
	}
	cfg := func(key string) map[string]any { return byKey[key].DesiredConfiguration }
	numEq := func(key, field string, want float64) {
		t.Helper()
		got, ok := num(cfg(key)[field])
		if !ok || got != want {
			t.Fatalf("%s.%s = %v, want %v", key, field, cfg(key)[field], want)
		}
	}
	strEq := func(key, field, want string) {
		t.Helper()
		if cfg(key)[field] != want {
			t.Fatalf("%s.%s = %v, want %v", key, field, cfg(key)[field], want)
		}
	}
	boolEq := func(key, field string, want bool) {
		t.Helper()
		if cfg(key)[field] != want {
			t.Fatalf("%s.%s = %v, want %v", key, field, cfg(key)[field], want)
		}
	}
	numEq("compute", "replicas", 2)
	numEq("compute", "cpu_millicores", 500)
	numEq("compute", "memory_mib", 512)
	strEq("compute", "orchestrator", "kubernetes")
	if _, stated := compOf(plan, "compute").DesiredConfiguration["availability_zones"]; stated {
		t.Fatal("availability_zones invented for a spec that omits it")
	}
	strEq("database", "engine", "postgresql")
	strEq("database", "major_version", "17")
	numEq("database", "storage_gib", 20)
	boolEq("database", "private_networking", true)
	boolEq("cache", "authoritative", false)
	boolEq("object-storage", "required", true)
	boolEq("object-storage", "versioning_required", true)
	strEq("queue", "delivery", "at_least_once")
	boolEq("queue", "duplicate_safe_consumer", true)
	if cfg("queue")["ordering"] != "no global ordering assumed" {
		t.Fatal("queue ordering assumption invented")
	}
	numEq("data-movement", "rpo_seconds", 30)
	numEq("data-movement", "rto_seconds", 900)
	boolEq("network", "private_data_plane", true)
	stages, _ := cfg("routing")["canary_stages"].([]int)
	if len(stages) != 6 || stages[1] != 1 || stages[5] != 100 {
		t.Fatalf("canary stages: %v", cfg("routing")["canary_stages"])
	}
	corr, _ := cfg("observability")["correlation"].([]string)
	if len(corr) != 3 || corr[0] != "run_id" {
		t.Fatalf("correlation: %v", corr)
	}
	boolEq("identity", "long_lived_credentials", false)
	boolEq("security", "long_lived_credentials", false)
}

// Fidelity: changed spec values flow through (nothing hardcoded that matters).
func TestPlanReflectsSpecChanges(t *testing.T) {
	reg := fullPassRegistry(t)
	spec := planSpec(t)
	spec["compute"].(map[string]any)["replicas"] = float64(4)
	spec["database"].(map[string]any)["storage_gib"] = float64(50)
	spec["requirements"].(map[string]any)["rpo_seconds"] = float64(60)
	spec["requirements"].(map[string]any)["availability_zones"] = float64(2)
	rep := Evaluate(spec, reg, "azure")
	plan := BuildPlan(spec, rep, reg)
	if got, _ := num(compOf(plan, "compute").DesiredConfiguration["replicas"]); got != 4 {
		t.Fatalf("replicas=%v", got)
	}
	if got, _ := num(compOf(plan, "database").DesiredConfiguration["storage_gib"]); got != 50 {
		t.Fatalf("storage=%v", got)
	}
	if got, _ := num(compOf(plan, "data-movement").DesiredConfiguration["rpo_seconds"]); got != 60 {
		t.Fatalf("rpo=%v", got)
	}
	if got, _ := num(compOf(plan, "compute").DesiredConfiguration["availability_zones"]); got != 2 {
		t.Fatalf("availability_zones=%v", got)
	}
	if !strings.Contains(compOf(plan, "data-movement").DesiredConfiguration["postgres_cdc"].(string), "lag<=60s") {
		t.Fatalf("cdc gate not derived from rpo: %v", compOf(plan, "data-movement").DesiredConfiguration)
	}
}

// Endpoint helpers.
func registerPlanWID(t *testing.T, key string) string {
	t.Helper()
	c, body := register(t, key)
	if c != 201 {
		t.Fatalf("setup register: %d", c)
	}
	var wl map[string]any
	_ = json.Unmarshal([]byte(body), &wl)
	id, _ := wl["id"].(string)
	return id
}

func evalCompat(t *testing.T, wid, key string) {
	evalCompatAs(t, wid, key, "test-admin")
}

func evalCompatAs(t *testing.T, wid, key, actor string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads/"+wid+"/compatibility", strings.NewReader(`{"target_provider":"azure"}`))
	req.Header.Set("Idempotency-Key", key)
	withBearer(req, actor, "")
	rec := httptest.NewRecorder()
	postCompat(rec, req, wid)
	if rec.Code != 202 {
		t.Fatalf("setup compat: %d %s", rec.Code, rec.Body.String())
	}
}

func postPlanReq(t *testing.T, wid, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads/"+wid+"/plan", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	withBearer(req, "test-admin", "")
	rec := httptest.NewRecorder()
	postPlan(rec, req, wid)
	return rec
}

// Endpoint: conditional plan end-to-end (live shape), persisted, replayed,
// no mutation, no migration records.
func TestPlanEndpoint(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "plankey0000000001")
	evalCompat(t, wid, "plankey0000000002")
	ms := store.(*MemStore)
	auditsBefore := len(ms.audits)

	rec := postPlanReq(t, wid, "plankey0000000003", `{"target_provider":"azure"}`)
	if rec.Code != 202 {
		t.Fatalf("POST plan: %d %s", rec.Code, rec.Body.String())
	}
	var plan TargetMigrationPlan
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.OverallStatus != "conditional" {
		t.Fatalf("overall=%s", plan.OverallStatus)
	}
	if plan.CompatibilityRegistryVersion != 1 || plan.PlannerVersion != PlannerVersion {
		t.Fatal("versions not recorded")
	}
	db := compOf(plan, "database")
	if db.ProvisioningMode != ModeGated || db.ValidationGate == "" {
		t.Fatalf("database gate missing: %+v", db)
	}
	stored, ok := store.GetLatestTargetPlan(wid)
	if !ok || stored.ID != plan.ID {
		t.Fatal("plan not persisted retrievable")
	}
	getReq := httptest.NewRequest(http.MethodGet, "/v1/workloads/"+wid+"/plan", nil)
	getRec := httptest.NewRecorder()
	getPlanLatest(getRec, getReq, wid)
	if getRec.Code != 200 {
		t.Fatalf("GET latest: %d", getRec.Code)
	}
	var wl map[string]any // N: lifecycle untouched
	wl, _ = store.GetWorkload(wid)
	if wl["lifecycle_state"] != "registered" {
		t.Fatalf("lifecycle=%v", wl["lifecycle_state"])
	}
	if len(ms.migrations) != 0 { // O
		t.Fatal("planning created migration records")
	}
	if len(ms.audits) != auditsBefore { // planning is analysis: no audit
		t.Fatal("planning created audit events")
	}
	rec2 := postPlanReq(t, wid, "plankey0000000004", `{"target_provider":"azure"}`) // repeat identical
	var plan2 TargetMigrationPlan
	_ = json.Unmarshal(rec2.Body.Bytes(), &plan2)
	if plan2.ID != plan.ID {
		t.Fatal("repeat plan diverged")
	}
	rec3 := postPlanReq(t, wid, "plankey0000000003", `{"target_provider":"azure"}`) // L: replay
	if rec3.Code != 202 || rec3.Body.String() != rec.Body.String() {
		t.Fatal("idempotent replay broken")
	}
	rec4 := postPlanReq(t, wid, "plankey0000000003", `{"target_provider":"azure","extra":1}`) // M: conflict
	if rec4.Code != 409 {
		t.Fatalf("expected 409, got %d", rec4.Code)
	}
}

// I: missing report -> explicit failure. J: unknown workload -> 404.
// K: invalid spec -> 400, never planned.
func TestPlanFailures(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "plankey0000000005")
	if rec := postPlanReq(t, wid, "plankey0000000006", `{"target_provider":"azure"}`); rec.Code != 409 { // I
		t.Fatalf("expected 409 STALE, got %d: %s", rec.Code, rec.Body.String())
	} else if !strings.Contains(rec.Body.String(), "COMPATIBILITY_STALE") {
		t.Fatalf("missing code: %s", rec.Body.String())
	}
	if rec := postPlanReq(t, "00000000-0000-0000-0000-000000000000", "plankey0000000007", `{"target_provider":"azure"}`); rec.Code != 404 { // J
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	ms := store.(*MemStore) // K: legacy {} row can never be planned
	legacyID := "22222222-3333-4444-8555-666666666666"
	_ = ms.CreateWorkload(legacyID, map[string]any{"id": legacyID, "name": "legacy",
		"schema_version": 1, "lifecycle_state": "registered", "canonical_spec": map[string]any{}})
	if rec := postPlanReq(t, legacyID, "plankey0000000008", `{"target_provider":"azure"}`); rec.Code != 400 { // K
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if _, found := ms.GetLatestTargetPlan(legacyID); found {
		t.Fatal("invalid workload produced a plan")
	}
}

// Stale report: spec changed after evaluation -> explicit 409, never assumed.
func TestPlanStaleReport(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "plankey0000000009")
	evalCompat(t, wid, "plankey0000000010")
	ms := store.(*MemStore)
	wl, _ := ms.GetWorkload(wid)
	spec := wl["canonical_spec"].(map[string]any)
	compute := spec["compute"].(map[string]any)
	compute["replicas"] = float64(5) // simulate spec drift after evaluation
	if rec := postPlanReq(t, wid, "plankey0000000011", `{"target_provider":"azure"}`); rec.Code != 409 {
		t.Fatalf("expected 409 STALE, got %d: %s", rec.Code, rec.Body.String())
	} else if !strings.Contains(rec.Body.String(), "COMPATIBILITY_STALE") {
		t.Fatalf("missing code: %s", rec.Body.String())
	}
}
