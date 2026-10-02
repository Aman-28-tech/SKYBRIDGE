// Compatibility engine tests (FR-005). Pure evaluator tests use injected
// registry copies; endpoint tests exercise POST/GET (run: go test ./...).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	reg, err := EmbeddedRegistry()
	if err != nil {
		t.Fatalf("embedded registry: %v", err)
	}
	return reg
}

func cloneRegistry(t *testing.T, reg *Registry) *Registry {
	t.Helper()
	b, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	var out Registry
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	out.byID = map[string]*Capability{}
	for i := range out.Capabilities {
		out.byID[out.Capabilities[i].ID] = &out.Capabilities[i]
	}
	return &out
}

func specMap(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(fullSpecBody()), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRegistryCopyMatchesCanonical(t *testing.T) {
	canon, err := os.ReadFile("../../packages/contracts/schemas/capability-registry.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(canon) != string(registryBytes) {
		t.Fatal("embedded registry copy drifted from canonical file; re-copy it")
	}
	reg := testRegistry(t)
	if reg.Version != 1 || len(reg.Capabilities) != 9 {
		t.Fatalf("version=%d capabilities=%d, want 1/9", reg.Version, len(reg.Capabilities))
	}
}

// A: all capabilities pass -> overall pass (private_networking gains the
// integration-test evidence it lacks in the example file).
func TestCompatAllPass(t *testing.T) {
	reg := cloneRegistry(t, testRegistry(t))
	c := reg.byID["postgresql.private_networking"]
	c.Evidence = append(c.Evidence, EvidenceRef{Type: "integration_test", Ref: "tests/probe.yaml"})
	rep := Evaluate(specMap(t), reg, "azure")
	if rep.Status != "pass" {
		t.Fatalf("status=%s checks=%+v", rep.Status, rep.Checks)
	}
	for _, c := range rep.Checks {
		if c.Status != "pass" {
			t.Fatalf("check %q: %s", c.Requirement, c.Status)
		}
	}
}

// B: one conditional + rest pass -> conditional (embedded registry as-is:
// private_networking is docs-only).
func TestCompatConditional(t *testing.T) {
	rep := Evaluate(specMap(t), testRegistry(t), "azure")
	if rep.Status != "conditional" {
		t.Fatalf("status=%s, want conditional", rep.Status)
	}
	found := false
	for _, c := range rep.Checks {
		if c.Status == "conditional" {
			found = true
			if len(c.Evidence) == 0 || len(c.Assumptions) == 0 {
				t.Fatal("conditional check lacks evidence/assumptions")
			}
		}
		if c.Status == "block" || c.Status == "unknown" {
			t.Fatalf("unexpected %s: %s", c.Status, c.Requirement)
		}
	}
	if !found {
		t.Fatal("no conditional check found")
	}
}

// C: one unknown + rest pass -> unknown.
func TestCompatUnknown(t *testing.T) {
	reg := cloneRegistry(t, testRegistry(t))
	kept := reg.Capabilities[:0]
	for _, c := range reg.Capabilities {
		if c.ID != "observability.metrics" {
			kept = append(kept, c)
		}
	}
	reg.Capabilities = kept
	delete(reg.byID, "observability.metrics")
	if rep := Evaluate(specMap(t), reg, "azure"); rep.Status != "unknown" {
		t.Fatalf("status=%s, want unknown", rep.Status)
	}
}

// D: one block + rest pass -> block.
func TestCompatBlock(t *testing.T) {
	reg := cloneRegistry(t, testRegistry(t))
	c := reg.byID["queue.at_least_once"]
	c.Supported = false
	if rep := Evaluate(specMap(t), reg, "azure"); rep.Status != "block" {
		t.Fatalf("status=%s, want block", rep.Status)
	}
}

// E: block + unknown -> block. F: unknown + conditional -> unknown.
func TestCompatAggregationPrecedence(t *testing.T) {
	mk := func() *Registry { return cloneRegistry(t, testRegistry(t)) }
	regE := mk()
	regE.byID["queue.at_least_once"].Supported = false
	kept := regE.Capabilities[:0]
	for _, c := range regE.Capabilities {
		if c.ID != "observability.metrics" {
			kept = append(kept, c)
		}
	}
	regE.Capabilities = kept
	delete(regE.byID, "observability.metrics")
	if rep := Evaluate(specMap(t), regE, "azure"); rep.Status != "block" {
		t.Fatalf("E: status=%s, want block", rep.Status)
	}

	regF := mk()
	kept = regF.Capabilities[:0]
	for _, c := range regF.Capabilities {
		if c.ID != "routing.weighted_canary" {
			kept = append(kept, c)
		}
	}
	regF.Capabilities = kept
	delete(regF.byID, "routing.weighted_canary")
	if rep := Evaluate(specMap(t), regF, "azure"); rep.Status != "unknown" {
		t.Fatalf("F: status=%s, want unknown", rep.Status)
	}
}

// G: missing evidence -> unknown, never pass (hand-built entry bypasses the
// loader, which itself rejects evidence-less entries per registry schema).
func TestCompatMissingEvidenceUnknown(t *testing.T) {
	reg := &Registry{Version: 1, byID: map[string]*Capability{}}
	for _, c := range testRegistry(t).Capabilities {
		cp := c
		cp.Evidence = nil
		reg.Capabilities = append(reg.Capabilities, cp)
		reg.byID[cp.ID] = &reg.Capabilities[len(reg.Capabilities)-1]
	}
	rep := Evaluate(specMap(t), reg, "azure")
	if rep.Status != "unknown" {
		t.Fatalf("status=%s, want unknown", rep.Status)
	}
}

// H: deterministic check ordering follows the fixed rule order.
func TestCompatOrdering(t *testing.T) {
	rep := Evaluate(specMap(t), testRegistry(t), "azure")
	want := []string{"compute:", "database: postgresql", "database: private", "network:",
		"object storage:", "queue:", "identity:", "routing:", "observability:"}
	if len(rep.Checks) != len(want) {
		t.Fatalf("checks=%d, want %d", len(rep.Checks), len(want))
	}
	for i, prefix := range want {
		if !strings.HasPrefix(rep.Checks[i].Requirement, prefix) {
			t.Fatalf("check %d: %q, want prefix %q", i, rep.Checks[i].Requirement, prefix)
		}
	}
}

// I: same inputs -> byte-identical report (deterministic ID included).
func TestCompatDeterministic(t *testing.T) {
	reg := testRegistry(t)
	spec := specMap(t)
	a := Evaluate(spec, reg, "azure")
	b := Evaluate(spec, reg, "azure")
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("nondeterministic:\n%s\n%s", ja, jb)
	}
}

// M: verdicts are evidence-backed, not name-based: renaming every service
// must change only target_capability prose, never statuses or evidence.
func TestCompatNotNameBased(t *testing.T) {
	reg := cloneRegistry(t, testRegistry(t))
	for i := range reg.Capabilities {
		reg.Capabilities[i].Service = "unrelated-service"
	}
	a := Evaluate(specMap(t), testRegistry(t), "azure")
	b := Evaluate(specMap(t), reg, "azure")
	if len(a.Checks) != len(b.Checks) {
		t.Fatal("check count changed")
	}
	for i := range a.Checks {
		if a.Checks[i].Status != b.Checks[i].Status {
			t.Fatalf("check %d status changed on rename: %s -> %s", i, a.Checks[i].Status, b.Checks[i].Status)
		}
		ja, _ := json.Marshal(a.Checks[i].Evidence)
		jb, _ := json.Marshal(b.Checks[i].Evidence)
		if string(ja) != string(jb) {
			t.Fatalf("check %d evidence changed on rename", i)
		}
	}
}

// Endpoint helpers.
func registerForCompat(t *testing.T, key string) string {
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

func postCompatReq(t *testing.T, wid, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads/"+wid+"/compatibility", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	withBearer(req, "test-admin", "")
	rec := httptest.NewRecorder()
	postCompat(rec, req, wid)
	return rec
}

// Endpoint: valid evaluation -> 202, persisted, GET latest matches, no mutation.
func TestCompatEndpoint(t *testing.T) {
	resetStore()
	wid := registerForCompat(t, "compatkey00000001")
	wlCount := len(store.ListWorkloads())
	rec := postCompatReq(t, wid, "compatkey00000002", `{"target_provider":"azure"}`)
	if rec.Code != 202 {
		t.Fatalf("POST: %d %s", rec.Code, rec.Body.String())
	}
	var rep CompatReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Status != "conditional" { // embedded registry: private_networking docs-only
		t.Fatalf("status=%s, want conditional", rep.Status)
	}
	if rep.RegistryVersion != 1 || rep.EvaluatorVersion != "compat-v1" { // L
		t.Fatalf("versions: %+v", rep)
	}
	if rep.ID == "" || rep.WorkloadID != wid {
		t.Fatalf("identity: %+v", rep)
	}
	stored, ok := store.GetLatestCompatReport(wid) // persistence
	if !ok || stored.ID != rep.ID {
		t.Fatal("report not persisted retrievable")
	}
	getReq := httptest.NewRequest(http.MethodGet, "/v1/workloads/"+wid+"/compatibility", nil)
	getRec := httptest.NewRecorder()
	getCompat(getRec, getReq, wid)
	if getRec.Code != 200 || getRec.Body.String() == "" {
		t.Fatalf("GET latest: %d", getRec.Code)
	}
	if len(store.ListWorkloads()) != wlCount { // no mutation
		t.Fatal("evaluation mutated workloads")
	}
	if ms, ok := store.(*MemStore); ok {
		for _, a := range ms.audits { // evaluations are analysis: no audit trail
			if a.Action == "evaluate_compatibility" {
				t.Fatal("evaluation should not create audit events")
			}
		}
	}
	// Repeat -> identical logical result.
	rec2 := postCompatReq(t, wid, "compatkey00000003", `{"target_provider":"azure"}`)
	var rep2 CompatReport
	_ = json.Unmarshal(rec2.Body.Bytes(), &rep2)
	if rep2.ID != rep.ID || rep2.Status != rep.Status {
		t.Fatal("repeat evaluation diverged")
	}
	// Idempotent replay + conflict.
	rec3 := postCompatReq(t, wid, "compatkey00000002", `{"target_provider":"azure"}`)
	if rec3.Code != 202 || rec3.Body.String() != rec.Body.String() {
		t.Fatal("idempotent replay broken")
	}
	rec4 := postCompatReq(t, wid, "compatkey00000002", `{"target_provider":"azure","target_region":"westus"}`)
	if rec4.Code != 409 {
		t.Fatalf("expected 409, got %d", rec4.Code)
	}
}

// J: unknown workload -> 404. K: invalid canonical_spec fails safe (400),
// including the legacy {} row which can never bypass checks.
func TestCompatUnknownAndInvalid(t *testing.T) {
	resetStore()
	rec := postCompatReq(t, "00000000-0000-0000-0000-000000000000", "compatkey00000004", `{"target_provider":"azure"}`)
	if rec.Code != 404 { // J
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	// Legacy row with canonical_spec {} persisted directly.
	ms := NewMemStore()
	prev := store
	store = ms
	defer func() { store = prev }()
	legacyID := "11111111-2222-4333-8444-555555555555"
	_ = ms.CreateWorkload(legacyID, map[string]any{"id": legacyID, "name": "legacy",
		"schema_version": 1, "lifecycle_state": "registered", "canonical_spec": map[string]any{}})
	rec2 := postCompatReq(t, legacyID, "compatkey00000005", `{"target_provider":"azure"}`)
	if rec2.Code != 400 { // K
		t.Fatalf("expected 400, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if _, found := ms.GetLatestCompatReport(legacyID); found {
		t.Fatal("invalid workload produced a persisted report")
	}
	// Non-azure target rejected.
	wid := registerForCompat(t, "compatkey00000006")
	rec3 := postCompatReq(t, wid, "compatkey00000007", `{"target_provider":"aws"}`)
	if rec3.Code != 400 {
		t.Fatalf("expected 400, got %d", rec3.Code)
	}
}
