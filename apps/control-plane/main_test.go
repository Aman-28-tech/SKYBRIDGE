// Unit tests: policy gate mirror, idempotent register, cutover audit,
// post-write rollback block, state transitions (run: go test ./...).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func resetStore() {
	store = NewMemStore()
	installTestFixtures()
}

func basePolicy() PolicyInput {
	return PolicyInput{Action: "shift_traffic", Environment: "dev", Resource: "cloudshop",
		ActorType: "human", ActorID: "operator", TargetWeight: 1, ReadOnlyCanary: true,
		WriteOwnership: "aws", ValidationStatus: "passed", CompatibilityStatus: "pass",
		RPOSeconds: 30, CDCLagSeconds: 8, TargetHealthy: true, Approval: "none"}
}

func TestPolicyAllowDevStages(t *testing.T) {
	for _, w := range []int{0, 1, 5, 25, 50} {
		in := basePolicy()
		in.TargetWeight = w
		if dec, reasons := policyDecide(in); dec != PolicyAllow {
			t.Fatalf("dev stage %d: %s %v", w, dec, reasons)
		}
	}
}

func TestPolicyDenies(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*PolicyInput)
		dec  string
		want string
	}{
		{"lag", func(p *PolicyInput) { p.CDCLagSeconds = 31 }, PolicyDeny, "cdc_lag_exceeds_rpo"},
		{"validation", func(p *PolicyInput) { p.ValidationStatus = "failed" }, PolicyDeny, "validation_failed"},
		{"conditional", func(p *PolicyInput) { p.CompatibilityStatus = "conditional" }, PolicyApprovalRequired, "approval"},
		{"unknown", func(p *PolicyInput) { p.CompatibilityStatus = "unknown" }, PolicyDeny, "compatibility_not_pass"},
		{"block", func(p *PolicyInput) { p.CompatibilityStatus = "block" }, PolicyDeny, "compatibility_not_pass"},
		{"drift", func(p *PolicyInput) { p.BlockingDriftCount = 1 }, PolicyDeny, "blocking_drift"},
		{"secDrift", func(p *PolicyInput) { p.SecurityCriticalDriftCount = 1 }, PolicyDeny, "security_critical_drift"},
		{"weight", func(p *PolicyInput) { p.TargetWeight = 10 }, PolicyDeny, "non_canonical_weight"},
		{"readOnly", func(p *PolicyInput) { p.ReadOnlyCanary = false }, PolicyDeny, "read_only_canary_violation"},
		{"staging50", func(p *PolicyInput) { p.Environment = "staging"; p.TargetWeight = 50 }, PolicyApprovalRequired, "approval"},
		{"prod", func(p *PolicyInput) { p.Environment = "production-like" }, PolicyApprovalRequired, "approval"},
		{"badAction", func(p *PolicyInput) { p.Action = "delete_database" }, PolicyDeny, "unsupported_action"},
		{"badEnv", func(p *PolicyInput) { p.Environment = "moon" }, PolicyDeny, "denied"},
	}
	for _, c := range cases {
		in := basePolicy()
		c.mut(&in)
		dec, reasons := policyDecide(in)
		if dec != c.dec || len(reasons) == 0 || !strings.Contains(strings.Join(reasons, " "), c.want) {
			t.Fatalf("%s: dec=%q reasons=%q want %q/%q", c.name, dec, reasons, c.dec, c.want)
		}
	}
}

func register(t *testing.T, key string) (int, string) {
	return registerAs(t, key, "test-admin")
}

func registerAs(t *testing.T, key, actor string) (int, string) {
	t.Helper()
	body := `{"schema_version":1,"name":"cloudshop","compute":{"orchestrator":"kubernetes","replicas":2,"cpu_millicores":500,"memory_mib":512},"database":{"engine":"postgresql","major_version":"17","storage_gib":20},"cache":{"engine":"redis","authoritative":false},"object_storage":{"required":true,"versioning_required":true},"queue":{"delivery_semantics":"at_least_once","duplicate_safe_consumer":true},"requirements":{"rpo_seconds":30,"rto_seconds":900,"private_data_plane":true}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	withBearer(req, actor, "")
	rec := httptest.NewRecorder()
	registerWorkload(rec, req)
	return rec.Code, rec.Body.String()
}

// normalizingStore wraps a Store and re-serializes saved idempotency bytes
// with different whitespace, modeling PostgreSQL JSONB normalization
// (which re-serializes stored documents on read). Handler replay must remain
// semantically equal even when the store does not preserve raw bytes.
type normalizingStore struct {
	Store
}

func (n *normalizingStore) SaveIdem(key, requestHash string, resp []byte, status int) {
	var v any
	if err := json.Unmarshal(resp, &v); err != nil {
		n.Store.SaveIdem(key, requestHash, resp, status)
		return
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		n.Store.SaveIdem(key, requestHash, resp, status)
		return
	}
	n.Store.SaveIdem(key, requestHash, pretty, status)
}

// Regression (live observation: 606 vs 656 bytes on PG replay): replay must be
// semantically equal — same status, deep-equal parsed JSON — even when the
// store normalizes the saved bytes. Byte identity is NOT required.
func TestIdemReplaySemanticDespiteNormalization(t *testing.T) {
	resetStore()
	store = &normalizingStore{Store: store}
	c1, b1 := register(t, "hhhhhhhhhhhhhhhh")
	if c1 != 201 {
		t.Fatalf("setup: %d", c1)
	}
	c2, b2 := register(t, "hhhhhhhhhhhhhhhh")
	if c2 != 201 {
		t.Fatalf("replay: %d", c2)
	}
	if b1 == b2 {
		t.Fatal("test is vacuous: normalizing wrapper did not alter bytes")
	}
	var j1, j2 any
	if err := json.Unmarshal([]byte(b1), &j1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(b2), &j2); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(j1, j2) {
		t.Fatalf("replay not semantically equal:\n%s\n%s", b1, b2)
	}
	if n := len(store.ListWorkloads()); n != 1 {
		t.Fatalf("expected 1 workload, got %d", n)
	}
}

func TestRegisterReplay(t *testing.T) {
	resetStore()
	c1, b1 := register(t, "0123456789abcdef")
	c2, b2 := register(t, "0123456789abcdef")
	if c1 != 201 || c2 != 201 || b1 != b2 {
		t.Fatalf("replay failed: %d/%d", c1, c2)
	}
	if n := len(store.ListWorkloads()); n != 1 {
		t.Fatalf("expected 1 workload, got %d", n)
	}
}

// A: first request succeeds and persists the full spec.
func TestIdemFirstRequestSucceeds(t *testing.T) {
	resetStore()
	code, body := register(t, "aaaaaaaaaaaaaaaa")
	if code != 201 {
		t.Fatalf("got %d: %s", code, body)
	}
	if n := len(store.ListWorkloads()); n != 1 {
		t.Fatalf("expected 1 workload, got %d", n)
	}
}

// C: same key + changed body -> 409 IDEMPOTENCY_CONFLICT, no second resource,
// denied audit recorded.
func TestIdemConflict(t *testing.T) {
	resetStore()
	c1, _ := register(t, "bbbbbbbbbbbbbbbb")
	if c1 != 201 {
		t.Fatalf("setup: %d", c1)
	}
	changed := strings.Replace(fullSpecBody(), `"replicas":2`, `"replicas":3`, 1)
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads", strings.NewReader(changed))
	req.Header.Set("Idempotency-Key", "bbbbbbbbbbbbbbbb")
	withBearer(req, "test-admin", "")
	rec := httptest.NewRecorder()
	registerWorkload(rec, req)
	if rec.Code != 409 {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "IDEMPOTENCY_CONFLICT") {
		t.Fatalf("missing code: %s", rec.Body.String())
	}
	if n := len(store.ListWorkloads()); n != 1 {
		t.Fatalf("conflict created a resource: %d workloads", n)
	}
	if ms, ok := store.(*MemStore); ok {
		found := false
		for _, a := range ms.audits {
			if a.Action == "register_workload" && a.Result == "denied" {
				found = true
			}
		}
		if !found {
			t.Fatal("conflict was not audited as denied")
		}
	}
}

// C (migrations): same key + changed body -> 409, no second migration.
func TestIdemConflictMigration(t *testing.T) {
	resetStore()
	_, wbody := register(t, "cccccccccccccccc")
	var wl map[string]any
	_ = json.Unmarshal([]byte(wbody), &wl)
	wid := wl["id"].(string)

	post := func(key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/migrations", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", key)
		withBearer(req, "test-admin", "")
		rec := httptest.NewRecorder()
		createMigration(rec, req)
		return rec
	}
	b1 := `{"workload_id":"` + wid + `","target_provider":"azure"}`
	r1 := post("dddddddddddddddd", b1)
	if r1.Code != 202 {
		t.Fatalf("setup: %d %s", r1.Code, r1.Body.String())
	}
	r2 := post("dddddddddddddddd", b1) // identical retry -> replay
	if r2.Code != 202 || r1.Body.String() != r2.Body.String() {
		t.Fatalf("replay failed: %d vs %d", r1.Code, r2.Code)
	}
	// changed body with a *valid* alternate (different workload id, same
	// shape) -> 409 conflict, not a second migration.
	_, wbody2 := register(t, "ccccccc2cccccccc")
	var wl2 map[string]any
	_ = json.Unmarshal([]byte(wbody2), &wl2)
	wid2 := wl2["id"].(string)
	r3 := post("dddddddddddddddd", `{"workload_id":"`+wid2+`","target_provider":"azure"}`)
	if r3.Code != 409 || !strings.Contains(r3.Body.String(), "IDEMPOTENCY_CONFLICT") {
		t.Fatalf("expected 409 conflict, got %d: %s", r3.Code, r3.Body.String())
	}
}

// D: stored hash round-trips (the property a process restart depends on);
// legacy rows without a hash replay without comparison.
func TestIdemHashRoundTripAndLegacy(t *testing.T) {
	resetStore()
	ms, ok := store.(*MemStore)
	if !ok {
		t.Skip("memstore-specific")
	}
	h, err := canonicalHash([]byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	ms.SaveIdem("k1", h, []byte(`{}`), 201)
	if _, _, got, found := ms.CheckIdem("k1"); !found || got != h {
		t.Fatalf("hash round-trip failed: found=%v hash=%q", found, got)
	}
	ms.SaveIdem("legacy", "", []byte(`{}`), 201) // pre-migration row
	if _, _, got, found := ms.CheckIdem("legacy"); !found || got != "" {
		t.Fatal("legacy row must replay with empty hash")
	}
}

// E: concurrent same-key identical submissions -> one resource, identical bodies.
func TestIdemConcurrentIdentical(t *testing.T) {
	resetStore()
	const n = 20
	codes := make([]int, n)
	bodies := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, b := register(t, "eeeeeeeeeeeeeeee")
			codes[i], bodies[i] = c, b
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if codes[i] != 201 || bodies[i] != bodies[0] {
			t.Fatalf("goroutine %d: code=%d body=%q", i, codes[i], bodies[i])
		}
	}
	if count := len(store.ListWorkloads()); count != 1 {
		t.Fatalf("expected 1 workload, got %d", count)
	}
}

// E: concurrent same-key different bodies -> exactly one winner, rest 409.
func TestIdemConcurrentConflicting(t *testing.T) {
	resetStore()
	const n = 10
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// distinct body per goroutine (unique name keeps it schema-plausible)
			body := `{"schema_version":1,"name":"cloudshop-` + string(rune('a'+i)) + `","compute":{"orchestrator":"kubernetes","replicas":2,"cpu_millicores":500,"memory_mib":512},"database":{"engine":"postgresql","major_version":"17","storage_gib":20},"cache":{"engine":"redis","authoritative":false},"object_storage":{"required":true,"versioning_required":true},"queue":{"delivery_semantics":"at_least_once","duplicate_safe_consumer":true},"requirements":{"rpo_seconds":30,"rto_seconds":900,"private_data_plane":true}}`
			req := httptest.NewRequest(http.MethodPost, "/v1/workloads", strings.NewReader(body))
			req.Header.Set("Idempotency-Key", "ffffffffffffffff")
			withBearer(req, "test-admin", "")
			rec := httptest.NewRecorder()
			registerWorkload(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	wins, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case 201:
			wins++
		case 409:
			conflicts++
		default:
			t.Fatalf("unexpected code %d", c)
		}
	}
	if wins != 1 || conflicts != n-1 {
		t.Fatalf("wins=%d conflicts=%d, want 1/%d", wins, conflicts, n-1)
	}
	if count := len(store.ListWorkloads()); count != 1 {
		t.Fatalf("expected 1 workload, got %d", count)
	}
}

// F: expired record behaves as absent (treated as a new request).
func TestIdemExpiredTreatedAsNew(t *testing.T) {
	resetStore()
	ms, ok := store.(*MemStore)
	if !ok {
		t.Skip("memstore-specific")
	}
	c1, _ := register(t, "gggggggggggggggg")
	if c1 != 201 {
		t.Fatalf("setup: %d", c1)
	}
	ms.mu.Lock()
	r := ms.idem["gggggggggggggggg"]
	r.expiresAt = time.Now().Add(-time.Hour)
	ms.idem["gggggggggggggggg"] = r
	ms.mu.Unlock()
	c2, _ := register(t, "gggggggggggggggg") // same body, expired -> new request
	if c2 != 201 {
		t.Fatalf("expected 201 for expired key, got %d", c2)
	}
	if n := len(store.ListWorkloads()); n != 2 {
		t.Fatalf("expected 2 workloads after expiry, got %d", n)
	}
}

func fullSpecBody() string {
	return `{"schema_version":1,"name":"cloudshop","compute":{"orchestrator":"kubernetes","replicas":2,"cpu_millicores":500,"memory_mib":512},"database":{"engine":"postgresql","major_version":"17","storage_gib":20},"cache":{"engine":"redis","authoritative":false},"object_storage":{"required":true,"versioning_required":true},"queue":{"delivery_semantics":"at_least_once","duplicate_safe_consumer":true},"requirements":{"rpo_seconds":30,"rto_seconds":900,"private_data_plane":true}}`
}

// Regression: the submitted canonical specification must be persisted as
// canonical_spec (DOMAIN_MODEL), not discarded (previously persisted as {}).
func TestCanonicalSpecPersisted(t *testing.T) {
	resetStore()
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads", strings.NewReader(fullSpecBody()))
	req.Header.Set("Idempotency-Key", "aaaa1111bbbb2222")
	withBearer(req, "test-admin", "")
	rec := httptest.NewRecorder()
	registerWorkload(rec, req)
	if rec.Code != 201 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("missing workload id")
	}
	// GET must return the persisted spec.
	getReq := httptest.NewRequest(http.MethodGet, "/v1/workloads/"+id, nil)
	getRec := httptest.NewRecorder()
	getWorkload(getRec, getReq)
	if getRec.Code != 200 {
		t.Fatalf("get: %d %s", getRec.Code, getRec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	cs, ok := got["canonical_spec"].(map[string]any)
	if !ok {
		t.Fatalf("canonical_spec missing or not an object: %s", getRec.Body.String())
	}
	for _, k := range []string{"schema_version", "compute", "database", "cache", "object_storage", "queue", "requirements"} {
		if _, ok := cs[k]; !ok {
			t.Fatalf("canonical_spec missing %q", k)
		}
	}
	compute := cs["compute"].(map[string]any)
	if compute["replicas"] != float64(2) {
		t.Fatalf("compute.replicas: %v", compute["replicas"])
	}
	reqs := cs["requirements"].(map[string]any)
	if reqs["rpo_seconds"] != float64(30) || reqs["rto_seconds"] != float64(900) {
		t.Fatalf("requirements: %v", reqs)
	}
}

func TestCutoverAuditTrail(t *testing.T) {
	resetStore()
	wid, migID := wiredPassSetup(t)

	// allowed cutover stage 1 (real stored evidence, no stubs)
	cutReq := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+migID+"/cutover",
		strings.NewReader(`{"target_weight":1}`))
	withBearer(cutReq, "test-admin", "")
	cutRec := httptest.NewRecorder()
	cutover(cutRec, cutReq)
	if cutRec.Code != 202 {
		t.Fatalf("cutover stage 1: %d %s", cutRec.Code, cutRec.Body.String())
	}
	if !strings.Contains(cutRec.Body.String(), "policy_input_hash") {
		t.Fatalf("allow response missing policy hash: %s", cutRec.Body.String())
	}
	// denied cutover stage 10
	badReq := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+migID+"/cutover",
		strings.NewReader(`{"target_weight":10}`))
	withBearer(badReq, "test-admin", "")
	badRec := httptest.NewRecorder()
	cutover(badRec, badReq)
	if badRec.Code != 409 {
		t.Fatalf("expected 409, got %d", badRec.Code)
	}
	// audit: register + create + allow + deny = 4, with bundle versions and hashes
	ms, ok := store.(*MemStore)
	if !ok {
		t.Fatal("expected MemStore")
	}
	if ms.AuditCount() != 4 {
		t.Fatalf("expected 4 audit records, got %d", ms.AuditCount())
	}
	for _, a := range ms.audits {
		if a.PolicyBundleVersion == "" {
			t.Fatalf("audit missing bundle version: %+v", a)
		}
	}
	_ = wid
}

// Conditional compatibility routes to approval_required (recorded accurately),
// and cutover still refuses without approval.
func TestCutoverApprovalRequired(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "cutovercondkey001")
	evalCompat(t, wid, "cutovercondkey002") // embedded registry -> conditional
	evalPlanForDrift(t, wid)
	evalDriftForPlan(t, wid, "clean")
	migID := createMig(t, wid)
	rec := cutStage(t, migID, `{"target_weight":1}`)
	if rec.Code != 409 {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "approval_required") {
		t.Fatalf("missing approval_required: %s", rec.Body.String())
	}
	ms := store.(*MemStore)
	found := false
	for _, a := range ms.audits {
		if a.Action == "shift_traffic" && a.PolicyDecision == "approval_required" && a.Result == "denied" {
			found = true
		}
	}
	if !found {
		t.Fatal("approval_required decision not audited accurately")
	}
}

func createMig(t *testing.T, wid string) string {
	return createMigAs(t, wid, "test-admin")
}

func createMigAs(t *testing.T, wid, actor string) string {
	t.Helper()
	creq := httptest.NewRequest(http.MethodPost, "/v1/migrations",
		strings.NewReader(`{"workload_id":"`+wid+`","target_provider":"azure"}`))
	creq.Header.Set("Idempotency-Key", "cutmigkey0000000"+wid[:4])
	withBearer(creq, actor, "")
	crec := httptest.NewRecorder()
	createMigration(crec, creq)
	if crec.Code != 202 {
		t.Fatalf("create migration: %d %s", crec.Code, crec.Body.String())
	}
	var run map[string]any
	_ = json.Unmarshal(crec.Body.Bytes(), &run)
	return run["id"].(string)
}

func cutStage(t *testing.T, migID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+migID+"/cutover", strings.NewReader(body))
	withBearer(req, "test-admin", "")
	rec := httptest.NewRecorder()
	cutover(rec, req)
	return rec
}

// Post-write rollback is blocked by the authoritative ownership fact
// (branch 1); unknown migrations are rejected outright (H-2 binding).
func TestPostWriteRollbackBlocked(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "rollblockreg00001")
	migID := createMig(t, wid)
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+migID+"/rollback",
		strings.NewReader(`{"write_ownership":"azure","reverse_sync_ready":false}`))
	withBearer(req, "test-admin", "")
	rec := httptest.NewRecorder()
	rollback(rec, req)
	// Pre-transfer (no azure fact, caller assertion without reverse sync) is
	// still refused by branch 2, as before.
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "POST_WRITE_ROLLBACK_BLOCKED") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	// Committed azure fact blocks authoritatively (branch 1).
	moved, err := store.TransferOwnership(migID, "aws", OwnershipRecord{
		MigrationID: migID, WorkloadID: wid, CurrentOwner: "azure", PreviousOwner: "aws",
	})
	if err != nil || !moved {
		t.Fatalf("setup commit: %v %v", moved, err)
	}
	req2 := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+migID+"/rollback",
		strings.NewReader(`{}`))
	withBearer(req2, "test-admin", "")
	rec2 := httptest.NewRecorder()
	rollback(rec2, req2)
	if rec2.Code != 409 || !strings.Contains(rec2.Body.String(), "POST_WRITE_ROLLBACK_BLOCKED") {
		t.Fatalf("got %d %s", rec2.Code, rec2.Body.String())
	}
	// Unknown migration is rejected, never accepted.
	req3 := httptest.NewRequest(http.MethodPost, "/v1/migrations/x/rollback",
		strings.NewReader(`{"write_ownership":"azure","reverse_sync_ready":false}`))
	withBearer(req3, "test-admin", "")
	rec3 := httptest.NewRecorder()
	rollback(rec3, req3)
	if rec3.Code != 404 {
		t.Fatalf("unknown migration rollback: %d %s", rec3.Code, rec3.Body.String())
	}
}

func TestTransitions(t *testing.T) {
	if !CanTransition("VALIDATING", "READY_FOR_CUTOVER") {
		t.Fatal("expected VALIDATING->READY_FOR_CUTOVER")
	}
	if CanTransition("READY_FOR_CUTOVER", "COMPLETED") {
		t.Fatal("must not skip cutover")
	}
	if !CanTransition("CUTTING_OVER", "ROLLING_BACK") {
		t.Fatal("expected CUTTING_OVER->ROLLING_BACK")
	}
	if CanTransition("COMPLETED", "ROLLING_BACK") {
		t.Fatal("terminal must not transition")
	}
	if !PostWriteRollbackBlocked("azure", false) || PostWriteRollbackBlocked("aws", false) {
		t.Fatal("post-write rule wrong")
	}
}
