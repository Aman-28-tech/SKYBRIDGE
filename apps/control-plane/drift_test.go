// Drift reconciliation tests (FR-017, detection only).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, name string) ObservedSnapshot {
	t.Helper()
	b, err := os.ReadFile("../../tests/fixtures/drift/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var snap ObservedSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

func driftPlan(t *testing.T) (TargetMigrationPlan, map[string]any) {
	t.Helper()
	reg := testRegistry(t)
	var spec map[string]any
	if err := json.Unmarshal([]byte(fullSpecBody()), &spec); err != nil {
		t.Fatal(err)
	}
	rep := Evaluate(spec, reg, "azure")
	return BuildPlan(spec, rep, reg), spec
}

func findingPaths(rep DriftReport) map[string]string {
	out := map[string]string{}
	for _, f := range rep.Findings {
		out[f.Path] = f.Severity
	}
	return out
}

// Fixture 1 / Test A: identical desired/observed -> zero findings, clear gate.
func TestDriftClean(t *testing.T) {
	plan, specBefore := driftPlan(t)
	before, _ := json.Marshal(plan)
	rep := EvaluateDrift(plan, loadFixture(t, "clean"))
	if len(rep.Findings) != 0 {
		t.Fatalf("findings: %+v", rep.Findings)
	}
	if rep.DriftGate != "clear" || rep.Severity != "informational" {
		t.Fatalf("gate=%s severity=%s", rep.DriftGate, rep.Severity)
	}
	after, _ := json.Marshal(plan)
	if string(before) != string(after) { // K: desired untouched
		t.Fatal("desired plan mutated")
	}
	if rep.DesiredReference != plan.ID || rep.PlanVersion != PlannerVersion ||
		rep.RegistryVersion != 1 || rep.EvaluatorVersion != DriftEvaluatorVersion { // versions
		t.Fatalf("versions: %+v", rep)
	}
	_ = specBefore
}

// B: missing required component -> blocking. C: config mismatch severity.
func TestDriftMissingAndMismatch(t *testing.T) {
	plan, _ := driftPlan(t)
	snap := loadFixture(t, "clean")
	delete(snap.Components, "database")
	rep := EvaluateDrift(plan, snap)
	if p := findingPaths(rep); p["database"] != "blocking" { // B
		t.Fatalf("paths: %v", p)
	}
	rep2 := EvaluateDrift(plan, loadFixture(t, "db-private-off")) // C
	p2 := findingPaths(rep2)
	if p2["database.private_networking"] != "blocking" || p2["network.private_data_plane"] != "blocking" {
		t.Fatalf("paths: %v", p2)
	}
	rep3 := EvaluateDrift(plan, loadFixture(t, "replicas-mismatch"))
	if p := findingPaths(rep3); p["compute.replicas"] != "blocking" {
		t.Fatalf("paths: %v", p)
	}
}

// D: identity/network violations -> security_critical.
func TestDriftSecurityCritical(t *testing.T) {
	plan, _ := driftPlan(t)
	p1 := findingPaths(EvaluateDrift(plan, loadFixture(t, "identity-static")))
	if p1["identity.long_lived_credentials"] != "security_critical" || p1["identity.least_privilege"] != "security_critical" {
		t.Fatalf("identity: %v", p1)
	}
	p2 := findingPaths(EvaluateDrift(plan, loadFixture(t, "network-open")))
	if p2["network.public_database_endpoints"] != "security_critical" || p2["network.isolation"] != "security_critical" {
		t.Fatalf("network: %v", p2)
	}
}

// Fixtures 6-9: versioning, queue, routing blocking; observability informational.
func TestDriftFixtures(t *testing.T) {
	plan, _ := driftPlan(t)
	if p := findingPaths(EvaluateDrift(plan, loadFixture(t, "versioning-off"))); p["object-storage.versioning_required"] != "blocking" {
		t.Fatalf("versioning: %v", p)
	}
	pq := findingPaths(EvaluateDrift(plan, loadFixture(t, "queue-semantics")))
	if pq["queue.delivery"] != "blocking" || pq["queue.duplicate_safe_consumer"] != "blocking" {
		t.Fatalf("queue: %v", pq)
	}
	pr := findingPaths(EvaluateDrift(plan, loadFixture(t, "routing-mismatch")))
	if pr["routing.origins"] != "blocking" || pr["routing.canary_stages"] != "blocking" {
		t.Fatalf("routing: %v", pr)
	}
	ro := EvaluateDrift(plan, loadFixture(t, "observability-gap")) // E
	if ro.DriftGate != "clear" {
		t.Fatalf("gate=%s, want clear", ro.DriftGate)
	}
	for _, f := range ro.Findings {
		if f.Severity != "informational" {
			t.Fatalf("observability finding not informational: %+v", f)
		}
	}
}

// Fixture 10: multi -> deterministic ordering + security_critical aggregation. F/G/H.
func TestDriftMulti(t *testing.T) {
	plan, _ := driftPlan(t)
	a := EvaluateDrift(plan, loadFixture(t, "multi"))
	b := EvaluateDrift(plan, loadFixture(t, "multi"))
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) { // F: aggregation deterministic; G: IDs deterministic
		t.Fatal("nondeterministic multi report")
	}
	if a.DriftGate != "security_critical" || a.Severity != "security_critical" {
		t.Fatalf("gate=%s severity=%s", a.DriftGate, a.Severity)
	}
	lastComp, lastPath := "", "" // H: component plan-order, then path
	order := map[string]int{}
	for i, c := range plan.Components {
		order[c.Key] = i
	}
	for _, f := range a.Findings {
		oc, onc := order[f.Component]
		ol, onl := order[lastComp]
		if (onc && onl && oc < ol) || (onc && !onl && lastComp != "" && f.Component < lastComp) ||
			(f.Component == lastComp && f.Path < lastPath) {
			t.Fatalf("out of order: %s/%s after %s/%s", f.Component, f.Path, lastComp, lastPath)
		}
		if f.FindingID == "" || f.RemediationHint == "" || len(f.Evidence) == 0 && f.Severity != "informational" {
			t.Fatalf("finding incomplete: %+v", f)
		}
		lastComp, lastPath = f.Component, f.Path
	}
}

// Fixture 11: unknown content recorded, never a false PASS.
func TestDriftUnknownContent(t *testing.T) {
	plan, _ := driftPlan(t)
	rep := EvaluateDrift(plan, loadFixture(t, "unknown-field"))
	if rep.DriftGate != "clear" {
		t.Fatalf("gate=%s", rep.DriftGate)
	}
	if len(rep.Findings) == 0 {
		t.Fatal("unknown content silently ignored")
	}
	for _, f := range rep.Findings {
		if f.Severity != "informational" {
			t.Fatalf("unknown content affected gates: %+v", f)
		}
	}
}

// Fixture 12: conditional prerequisite unsatisfied -> blocking.
func TestDriftPrereqUnsatisfied(t *testing.T) {
	plan, _ := driftPlan(t)
	rep := EvaluateDrift(plan, loadFixture(t, "prereq-unsatisfied"))
	if p := findingPaths(rep); p["database.prerequisite_validated"] != "blocking" {
		t.Fatalf("paths: %v", p)
	}
	if rep.DriftGate != "blocking" {
		t.Fatalf("gate=%s", rep.DriftGate)
	}
}

// I/J: identical inputs identical report; changed observed changes report.
func TestDriftReportIdentity(t *testing.T) {
	plan, _ := driftPlan(t)
	a := EvaluateDrift(plan, loadFixture(t, "clean"))
	b := EvaluateDrift(plan, loadFixture(t, "clean"))
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) { // I
		t.Fatal("same inputs diverged")
	}
	c := EvaluateDrift(plan, loadFixture(t, "replicas-mismatch")) // J
	if string(ja) == mustJSON(t, c) {
		t.Fatal("changed observed did not change report")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Mapping: every field the live incident listed must exist in current
// planner output with the specified value. A field missing here explains an
// "unmodeled" finding as stale desired state, never as engine behavior.
func TestDriftDesiredFieldMapping(t *testing.T) {
	reg := testRegistry(t)
	spec := planSpec(t)
	rep := Evaluate(spec, reg, "azure")
	plan := BuildPlan(spec, rep, reg)
	byKey := map[string]PlanComponent{}
	for _, c := range plan.Components {
		byKey[c.Key] = c
	}
	get := func(key, field string) (any, bool) {
		v, ok := byKey[key].DesiredConfiguration[field]
		return v, ok
	}
	cases := []struct {
		key, field string
		want       any
	}{
		{"compute", "cpu_millicores", float64(500)},
		{"compute", "memory_mib", float64(512)},
		{"compute", "replicas", float64(2)},
		{"database", "publicly_accessible", false},
		{"object-storage", "required", true},
		{"object-storage", "versioning_required", true},
		{"object-storage", "version_id_identity", false},
		{"network", "isolation", "default-deny"},
		{"network", "public_database_endpoints", false},
		{"observability", "metrics", true},
		{"observability", "logs", true},
		{"observability", "traces", true},
		{"security", "audit_export", true},
		{"security", "encryption_at_rest", true},
		{"security", "tls", true},
	}
	for _, c := range cases {
		got, ok := get(c.key, c.field)
		if !ok {
			t.Fatalf("%s.%s absent from desired state (would surface as false unmodeled)", c.key, c.field)
		}
		if !jsonEqual(got, c.want) {
			t.Fatalf("%s.%s = %v, want %v", c.key, c.field, got, c.want)
		}
	}
}

// Stale planner output can never serve as desired state: a v1-era plan row
// (exactly the live incident shape) is rejected explicitly.
func TestDriftStalePlannerVersion(t *testing.T) {
	resetStore()
	wid := driftWID(t, "driftstalekey001")
	ms := store.(*MemStore)
	plan, _ := ms.GetLatestTargetPlan(wid)
	plan.PlannerVersion = "planner-v1" // simulate pre-fidelity row
	_ = ms.SaveTargetPlan(plan)
	rec := postDriftReq(t, wid, "driftstalekey002", fixtureBody(t, "clean"))
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "PLAN_STALE") {
		t.Fatalf("expected 409 PLAN_STALE, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Isolation semantics: exact default-deny equivalence is clean;
// genuine weakening is security_critical. No prose fuzzy-matching.
func TestDriftNetworkIsolationSemantics(t *testing.T) {
	plan, _ := driftPlan(t)
	base := loadFixture(t, "clean")
	if rep := EvaluateDrift(plan, base); len(rep.Findings) != 0 {
		t.Fatalf("equivalent default-deny must be clean: %+v", rep.Findings)
	}
	weak := loadFixture(t, "clean")
	weak.Components["network"]["isolation"] = "allow-all"
	rep := EvaluateDrift(plan, weak)
	found := false
	for _, f := range rep.Findings {
		if f.Path == "network.isolation" {
			found = true
			if f.Severity != "security_critical" {
				t.Fatalf("weakened isolation: severity=%s", f.Severity)
			}
		}
	}
	if !found {
		t.Fatal("weakened isolation produced no finding")
	}
	if rep.DriftGate != "security_critical" {
		t.Fatalf("gate=%s", rep.DriftGate)
	}
}

// Desired-known fields must never be reported as unmodeled: any
// "unmodeled" expectation must correspond to a genuinely absent desired field.
func TestDriftNoFalseUnmodeled(t *testing.T) {
	plan, _ := driftPlan(t)
	desired := map[string]map[string]any{}
	for _, c := range plan.Components {
		desired[c.Key] = c.DesiredConfiguration
	}
	for _, name := range []string{"clean", "multi", "unknown-field", "replicas-mismatch", "versioning-off"} {
		rep := EvaluateDrift(plan, loadFixture(t, name))
		for _, f := range rep.Findings {
			if f.ExpectedValue != "unmodeled" {
				continue
			}
			parts := strings.SplitN(f.Path, ".", 2)
			if len(parts) != 2 {
				continue // component-level unknown: genuinely unmodeled
			}
			if _, ok := desired[parts[0]][parts[1]]; ok {
				t.Fatalf("%s: desired-known field reported unmodeled (stale-plan symptom)", f.Path)
			}
		}
	}
}

// Recency refresh (live incident regression): re-observing an identical
// state refreshes recency without duplicating the row, and the refreshed
// state becomes latest even when a different state was seen in between.
func TestDriftRecencyRefresh(t *testing.T) {
	resetStore()
	plan, _ := driftPlan(t)
	clean1 := EvaluateDrift(plan, loadFixture(t, "clean"))
	blocking := EvaluateDrift(plan, loadFixture(t, "versioning-off"))
	if err := store.SaveDriftReport(clean1); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDriftReport(blocking); err != nil {
		t.Fatal(err)
	}
	cleanBefore, _ := json.Marshal(clean1)
	// Re-save identical clean state (same deterministic ID).
	clean2 := EvaluateDrift(plan, loadFixture(t, "clean"))
	if clean2.ID != clean1.ID {
		t.Fatal("deterministic ID changed for identical inputs")
	}
	if err := store.SaveDriftReport(clean2); err != nil {
		t.Fatal(err)
	}
	ms := store.(*MemStore)
	if len(ms.drifts) != 2 { // E: no duplicate row
		t.Fatalf("rows=%d, want 2", len(ms.drifts))
	}
	seen := map[string]int{}
	for _, id := range ms.driftOrder { // no duplicate order entries
		seen[id]++
		if seen[id] > 1 {
			t.Fatalf("duplicate order entry %s", id)
		}
	}
	latest, ok := store.GetLatestDriftReport(plan.WorkloadID, plan.ID) // A
	if !ok || latest.ID != clean1.ID {
		t.Fatalf("latest=%v ok=%v, want clean %s", latest.ID, ok, clean1.ID)
	}
	after, _ := json.Marshal(ms.drifts[clean1.ID]) // C: content unchanged
	if string(after) != string(cleanBefore) {
		t.Fatal("refresh altered clean content")
	}
	blk, ok := ms.drifts[blocking.ID] // D: blocking intact
	if !ok {
		t.Fatal("blocking row lost")
	}
	blkBefore, _ := json.Marshal(blocking)
	blkAfter, _ := json.Marshal(blk)
	if string(blkBefore) != string(blkAfter) {
		t.Fatal("blocking row altered")
	}
}

// Endpoint recency (F+G): blocking -> clean(new key) -> readiness clear-path,
// then blocking again -> readiness deny. Full live sequence.
func TestDriftEndpointRecencyFlow(t *testing.T) {
	resetStore()
	wid := driftWID(t, "recencyflowkey01")
	if rec := postDriftReq(t, wid, "recencyflowkey02", fixtureBody(t, "versioning-off")); rec.Code != 202 {
		t.Fatalf("blocking drift: %d", rec.Code)
	}
	rec := postDriftReq(t, wid, "recencyflowkey03", fixtureBody(t, "clean"))
	var rep DriftReport
	_ = json.Unmarshal(rec.Body.Bytes(), &rep)
	if rec.Code != 202 || rep.DriftGate != "clear" {
		t.Fatalf("clean re-observe: %d %s", rec.Code, rec.Body.String())
	}
	ms := store.(*MemStore)
	if len(ms.drifts) != 2 { // H: two states, two rows
		t.Fatalf("rows=%d, want 2", len(ms.drifts))
	}
	ready := postReadinessReq(t, wid, "recencyflowkey04", `{"target_weight":1}`)
	var dec map[string]any
	_ = json.Unmarshal(ready.Body.Bytes(), &dec)
	if ready.Code != 200 || dec["decision"] != PolicyApprovalRequired { // F
		t.Fatalf("readiness after clean: %d %v", ready.Code, dec["decision"])
	}
	if rec := postDriftReq(t, wid, "recencyflowkey05", fixtureBody(t, "versioning-off")); rec.Code != 202 {
		t.Fatalf("blocking again: %d", rec.Code)
	}
	ready2 := postReadinessReq(t, wid, "recencyflowkey06", `{"target_weight":1}`)
	var dec2 map[string]any
	_ = json.Unmarshal(ready2.Body.Bytes(), &dec2)
	if ready2.Code != 200 || dec2["decision"] != PolicyDeny { // G
		t.Fatalf("readiness after blocking: %d %v", ready2.Code, dec2["decision"])
	}
	if reasons, _ := dec2["reasons"].([]any); len(reasons) == 0 || !strings.Contains(strings.Join(toStrings(reasons), " "), "blocking_drift") {
		t.Fatalf("reasons=%v", dec2["reasons"])
	}
	if len(ms.migrations) != 0 {
		t.Fatal("drift flow created migrations")
	}
	wl, _ := ms.GetWorkload(wid)
	if wl["lifecycle_state"] != "registered" {
		t.Fatal("lifecycle mutated")
	}
}

func toStrings(v []any) []string {
	out := make([]string, 0, len(v))
	for _, x := range v {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Redaction: secret-looking paths never leak values.
func TestDriftRedaction(t *testing.T) {
	plan, _ := driftPlan(t)
	snap := loadFixture(t, "clean")
	snap.Components["database"]["backup_password"] = "s3cr3t"
	rep := EvaluateDrift(plan, snap)
	for _, f := range rep.Findings {
		if f.Path == "database.backup_password" {
			if f.ObservedValue != "[REDACTED]" {
				t.Fatalf("leaked: %v", f.ObservedValue)
			}
			return
		}
	}
	t.Fatal("redaction finding missing")
}

// Endpoint setup: register -> compat -> plan.
func driftWID(t *testing.T, key string) string {
	t.Helper()
	c, body := register(t, key)
	if c != 201 {
		t.Fatalf("setup register: %d", c)
	}
	var wl map[string]any
	_ = json.Unmarshal([]byte(body), &wl)
	id, _ := wl["id"].(string)
	// evaluate compat then plan through handlers
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads/"+id+"/compatibility", strings.NewReader(`{"target_provider":"azure"}`))
	req.Header.Set("Idempotency-Key", key+"c")
	withBearer(req, "test-admin", "")
	rec := httptest.NewRecorder()
	postCompat(rec, req, id)
	if rec.Code != 202 {
		t.Fatalf("setup compat: %d", rec.Code)
	}
	req2 := httptest.NewRequest(http.MethodPost, "/v1/workloads/"+id+"/plan", strings.NewReader(`{"target_provider":"azure"}`))
	req2.Header.Set("Idempotency-Key", key+"p")
	withBearer(req2, "test-admin", "")
	rec2 := httptest.NewRecorder()
	postPlan(rec2, req2, id)
	if rec2.Code != 202 {
		t.Fatalf("setup plan: %d %s", rec2.Code, rec2.Body.String())
	}
	return id
}

func postDriftReq(t *testing.T, wid, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads/"+wid+"/drift", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	withBearer(req, "test-admin", "")
	rec := httptest.NewRecorder()
	postDrift(rec, req, wid)
	return rec
}

func fixtureBody(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../tests/fixtures/drift/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Endpoint: clean -> 202 clear + persisted + listed; no mutation/migration/audit.
func TestDriftEndpoint(t *testing.T) {
	resetStore()
	wid := driftWID(t, "driftkey00000001")
	ms := store.(*MemStore)
	wlBefore, _ := ms.GetWorkload(wid)
	auditsBefore := len(ms.audits)

	rec := postDriftReq(t, wid, "driftkey00000002", fixtureBody(t, "clean"))
	if rec.Code != 202 {
		t.Fatalf("POST drift: %d %s", rec.Code, rec.Body.String())
	}
	var rep DriftReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.DriftGate != "clear" || len(rep.Findings) != 0 {
		t.Fatalf("gate=%s findings=%d", rep.DriftGate, len(rep.Findings))
	}
	stored := store.GetDriftReports(wid, "")
	if len(stored) != 1 || stored[0].ID != rep.ID {
		t.Fatal("report not persisted retrievable")
	}
	getReq := httptest.NewRequest(http.MethodGet, "/v1/workloads/"+wid+"/drift?status=open", nil)
	getRec := httptest.NewRecorder()
	getDriftList(getRec, getReq, wid)
	if getRec.Code != 200 || !strings.Contains(getRec.Body.String(), rep.ID) {
		t.Fatalf("GET list: %d", getRec.Code)
	}
	wlAfter, _ := ms.GetWorkload(wid) // L
	if mustJSON(t, wlBefore) != mustJSON(t, wlAfter) {
		t.Fatal("workload mutated")
	}
	if len(ms.migrations) != 0 { // M
		t.Fatal("drift created migrations")
	}
	if len(ms.audits) != auditsBefore {
		t.Fatal("drift created audit events")
	}
	rec2 := postDriftReq(t, wid, "driftkey00000003", fixtureBody(t, "replicas-mismatch"))
	var rep2 DriftReport
	_ = json.Unmarshal(rec2.Body.Bytes(), &rep2)
	if rec2.Code != 202 || rep2.DriftGate != "blocking" {
		t.Fatalf("drift case: %d %s", rec2.Code, rec2.Body.String())
	}
	if rec2.Body.String() == rec.Body.String() {
		t.Fatal("changed observed did not change report")
	}
	rec3 := postDriftReq(t, wid, "driftkey00000002", fixtureBody(t, "clean")) // S replay
	if rec3.Code != 202 || rec3.Body.String() != rec.Body.String() {
		t.Fatal("idempotent replay broken")
	}
	rec4 := postDriftReq(t, wid, "driftkey00000002", fixtureBody(t, "replicas-mismatch")) // T conflict
	if rec4.Code != 409 {
		t.Fatalf("expected 409, got %d", rec4.Code)
	}
}

// O/P/Q/R: unknown workload, missing plan, invalid snapshot, stale plan.
func TestDriftFailures(t *testing.T) {
	resetStore()
	rec := postDriftReq(t, "00000000-0000-0000-0000-000000000000", "driftkey00000004", fixtureBody(t, "clean"))
	if rec.Code != 404 { // O
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	wid := registerPlanWID(t, "driftkey00000005")
	if rec := postDriftReq(t, wid, "driftkey00000006", fixtureBody(t, "clean")); rec.Code != 404 { // P
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	wid2 := driftWID(t, "driftkey00000007")
	for name, body := range map[string]string{ // Q
		"bad-version": `{"snapshot_version":99,"components":{"compute":{}}}`,
		"empty":       `{"snapshot_version":1,"components":{}}`,
		"malformed":   `{"snapshot_version":`,
	} {
		if rec := postDriftReq(t, wid2, "driftkey-q-"+name+"-000", body); rec.Code != 400 {
			t.Fatalf("%s: expected 400, got %d", name, rec.Code)
		}
	}
	// R: stale plan after spec drift.
	ms := store.(*MemStore)
	wl, _ := ms.GetWorkload(wid2)
	spec := wl["canonical_spec"].(map[string]any)
	spec["compute"].(map[string]any)["replicas"] = float64(9)
	if rec := postDriftReq(t, wid2, "driftkey00000008", fixtureBody(t, "clean")); rec.Code != 409 ||
		!strings.Contains(rec.Body.String(), "PLAN_STALE") {
		t.Fatalf("expected 409 PLAN_STALE, got %d: %s", rec.Code, rec.Body.String())
	}
}

// N: evaluation independent of environment.
func TestDriftEnvIndependent(t *testing.T) {
	plan, _ := driftPlan(t)
	snap := loadFixture(t, "clean")
	a := mustJSON(t, EvaluateDrift(plan, snap))
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("SKYBRIDGE_FUTURE_CLOUD", "true")
	if b := mustJSON(t, EvaluateDrift(plan, snap)); a != b {
		t.Fatal("evaluation depends on environment")
	}
}
