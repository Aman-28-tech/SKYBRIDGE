// Targeted security-remediation tests (H-1, H-2, H-3, M-4).
//
// Covers exactly the remediation scope, with small targeted tests only:
//  1. no credentials -> 401
//  2. forged actor headers -> 401 (headers never confer identity)
//  3. authenticated wrong actor -> 403
//  4. unauthorized workload -> 403
//  5. unauthorized migration -> 403
//  6. migration/workload mismatch -> rejected
//  7. unauthorized approval -> 403
//  8. unauthorized execute -> 403
//  9. unauthorized cutover -> 403
//  10. unauthorized ownership (CloudShop admin: see workloads/cloudshop/api/admin_auth_test.go)
//  11. ownership concurrency -> one winner
//  12. ownership restart/recovery -> safe converge (plus CloudShop file-restart test)
//  13. audit actor forgery -> audit carries the verified principal
//  14. approval/execute with valid authenticated actor -> allowed
//  15. existing valid local demo path (demo actor pair end-to-end)
//
// Test doubles, NOT secrets: tokens below are deterministic fixtures for
// in-process tests only. Production code defines no default tokens, and
// demo scripts mint per-run random tokens. Fixture properties:
//
//	test-admin  human admin  (may act on any workload; preserves pre-existing tests)
//	requester-1 human admin  (demo requester)
//	approver-1  human admin  (demo approver)
//	approver-2  human admin  (second approver in pre-existing tests)
//	test-owner  human        (owns workloads it registers; non-admin)
//	test-other  human        (owns nothing; non-admin)
//	test-agent  agent        (can never decide approvals; non-admin)
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func installTestFixtures() {
	setTestPrincipals([]tokenEntry{
		{Token: "test-token-test-admin", ActorID: "test-admin", ActorType: "human", Admin: true},
		{Token: "test-token-requester-1", ActorID: "requester-1", ActorType: "human", Admin: true},
		{Token: "test-token-approver-1", ActorID: "approver-1", ActorType: "human", Admin: true},
		{Token: "test-token-approver-2", ActorID: "approver-2", ActorType: "human", Admin: true},
		{Token: "test-token-test-owner", ActorID: "test-owner", ActorType: "human"},
		{Token: "test-token-test-other", ActorID: "test-other", ActorType: "human"},
		{Token: "test-token-test-agent", ActorID: "test-agent", ActorType: "agent"},
	})
}

// bearerFor returns the fixture token for a test actor, registering unknown
// names on first use as non-admin humans (or agents when typed so).
// Fail-closed default: unknowns never gain admin.
func bearerFor(actor, actorType string) string {
	if actor == "" {
		return ""
	}
	typ := actorType
	if typ == "" {
		typ = "human"
	}
	authMu.Lock()
	defer authMu.Unlock()
	tok := "test-token-" + actor
	if _, ok := authTokens[tok]; !ok {
		authTokens[tok] = Principal{ID: actor, Type: typ}
	}
	return tok
}

// withBearer attaches fixture credentials for actor ("" = none).
func withBearer(req *http.Request, actor, actorType string) *http.Request {
	if tok := bearerFor(actor, actorType); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return req
}

// ownerChain builds register -> compat -> plan -> drift -> migration as
// test-owner (non-admin), returning owner workload + migration IDs.
func ownerChain(t *testing.T, key string) (wid, migID string) {
	t.Helper()
	resetStore()
	c, body := registerAs(t, key+"reg00001", "test-owner")
	if c != 201 {
		t.Fatalf("owner register: %d %s", c, body)
	}
	var wl map[string]any
	_ = json.Unmarshal([]byte(body), &wl)
	wid, _ = wl["id"].(string)
	if wl["owner_id"] != "test-owner" {
		t.Fatalf("owner not recorded: %v", wl["owner_id"])
	}
	evalCompatAs(t, wid, key+"cmp00001", "test-owner")
	evalPlanForDriftAs(t, wid, "test-owner")
	evalDriftForPlan(t, wid, "clean")
	migID = createMigAs(t, wid, "test-owner")
	return wid, migID
}

// ownerApprovalChain extends ownerChain with a pending approval requested by
// the owner (conditional evidence via approvalChain-style fixtures is not
// needed: postApproval requires approval_required, which the demo fixture
// compat (conditional) yields).
func ownerApproval(t *testing.T, wid, key string) Approval {
	t.Helper()
	rec := postApprovalReq(t, wid, key+"oapr0001", `{"target_weight":1}`, "test-owner", "")
	if rec.Code != 201 {
		t.Fatalf("owner approval request: %d %s", rec.Code, rec.Body.String())
	}
	return decodeApproval(t, rec)
}

// 1. No credentials -> 401 (fail-closed, no default identity).
func TestAuthNoCredentials(t *testing.T) {
	resetStore()
	body := `{"schema_version":1,"name":"cloudshop","compute":{"orchestrator":"kubernetes","replicas":2,"cpu_millicores":500,"memory_mib":512},"database":{"engine":"postgresql","major_version":"17","storage_gib":20},"cache":{"engine":"redis","authoritative":false},"object_storage":{"required":true,"versioning_required":true},"queue":{"delivery_semantics":"at_least_once","duplicate_safe_consumer":true},"requirements":{"rpo_seconds":30,"rto_seconds":900,"private_data_plane":true}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", "nocredkey00000001")
	rec := httptest.NewRecorder()
	registerWorkload(rec, req)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "UNAUTHENTICATED") {
		t.Fatalf("no credentials: %d %s", rec.Code, rec.Body.String())
	}
	// Unknown migration without credentials is 401 (never existence-oracle).
	req2 := httptest.NewRequest(http.MethodPost, "/v1/migrations/nope/rollback", strings.NewReader(`{}`))
	rec2 := httptest.NewRecorder()
	rollback(rec2, req2)
	if rec2.Code != 401 {
		t.Fatalf("no credentials rollback: %d", rec2.Code)
	}
}

// 2. Forged actor headers confer nothing: without valid bearer material the
// mutation is 401, and with another principal's bearer the audit binds the
// bearer identity (never the headers).
func TestAuthForgedHeaders(t *testing.T) {
	resetStore()
	body := `{"schema_version":1,"name":"cloudshop","compute":{"orchestrator":"kubernetes","replicas":2,"cpu_millicores":500,"memory_mib":512},"database":{"engine":"postgresql","major_version":"17","storage_gib":20},"cache":{"engine":"redis","authoritative":false},"object_storage":{"required":true,"versioning_required":true},"queue":{"delivery_semantics":"at_least_once","duplicate_safe_consumer":true},"requirements":{"rpo_seconds":30,"rto_seconds":900,"private_data_plane":true}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", "forgedkey00000001")
	req.Header.Set("X-Actor-Id", "test-owner")
	req.Header.Set("X-Actor-Type", "human")
	rec := httptest.NewRecorder()
	registerWorkload(rec, req)
	if rec.Code != 401 {
		t.Fatalf("forged headers without bearer: %d %s", rec.Code, rec.Body.String())
	}
	// Garbage bearer + owner headers is still 401.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/workloads", strings.NewReader(body))
	req2.Header.Set("Idempotency-Key", "forgedkey00000002")
	req2.Header.Set("Authorization", "Bearer not-a-real-token")
	req2.Header.Set("X-Actor-Id", "test-admin")
	rec2 := httptest.NewRecorder()
	registerWorkload(rec2, req2)
	if rec2.Code != 401 {
		t.Fatalf("garbage bearer: %d", rec2.Code)
	}
}

// 3. Authenticated wrong actor: test-other cannot decide test-owner's
// approval (403), and the owner cannot self-decide (403, distinct reason).
func TestAuthWrongActorDecide(t *testing.T) {
	wid, _ := ownerChain(t, "wrongactor000001")
	a := ownerApproval(t, wid, "wrongactor000001")
	// Stranger: 403 FORBIDDEN (not authorized for the workload).
	rec := decideReq(t, wid, a.ID, "wrongactorkey0001", "approved", "test-other", "")
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "FORBIDDEN") {
		t.Fatalf("stranger decide: %d %s", rec.Code, rec.Body.String())
	}
	// Owner self-decide: 403 APPROVAL_FORBIDDEN (requester != decider).
	rec2 := decideReq(t, wid, a.ID, "wrongactorkey0002", "approved", "test-owner", "")
	if rec2.Code != 403 || !strings.Contains(rec2.Body.String(), "APPROVAL_FORBIDDEN") {
		t.Fatalf("self decide: %d %s", rec2.Code, rec.Body.String())
	}
	// Agent principal (verified type agent) can never decide: 403.
	rec3 := decideReq(t, wid, a.ID, "wrongactorkey0003", "approved", "test-agent", "")
	if rec3.Code != 403 {
		t.Fatalf("agent decide: %d %s", rec3.Code, rec.Body.String())
	}
}

// 4. Unauthorized workload: test-other cannot run workload-scoped mutations
// on test-owner's workload.
func TestAuthUnauthorizedWorkload(t *testing.T) {
	wid, _ := ownerChain(t, "unauthwl000000001")
	for name, fn := range map[string]func() *httptest.ResponseRecorder{
		"compat": func() *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "/v1/workloads/"+wid+"/compatibility",
				strings.NewReader(`{"target_provider":"azure"}`))
			req.Header.Set("Idempotency-Key", "unauthwlkey000001")
			withBearer(req, "test-other", "")
			rec := httptest.NewRecorder()
			postCompat(rec, req, wid)
			return rec
		},
		"readiness": func() *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "/v1/workloads/"+wid+"/readiness",
				strings.NewReader(`{"target_weight":1}`))
			req.Header.Set("Idempotency-Key", "unauthwlkey000002")
			withBearer(req, "test-other", "")
			rec := httptest.NewRecorder()
			postReadiness(rec, req, wid)
			return rec
		},
		"request-approval": func() *httptest.ResponseRecorder {
			return postApprovalReqAs(t, wid, "unauthwlkey000003", `{"target_weight":1}`, "test-other", "")
		},
	} {
		if rec := fn(); rec.Code != 403 {
			t.Fatalf("%s: got %d %s, want 403", name, rec.Code, rec.Body.String())
		}
	}
}

// 5+8. Unauthorized migration/execute: test-other cannot execute test-owner's
// migration (403, before any policy or Temporal work).
func TestAuthUnauthorizedMigrationExecute(t *testing.T) {
	_, migID := ownerChain(t, "unauthmig00000001")
	rec := postExecuteReqAs(t, migID, "unauthmigkey00001", `{"target_weight":1}`, "test-other", "")
	if rec.Code != 403 {
		t.Fatalf("unauthorized execute: %d %s", rec.Code, rec.Body.String())
	}
}

// 6. Migration/workload mismatch is rejected: migA bound to widA cannot be
// driven through widB's route.
func TestAuthMigrationWorkloadMismatch(t *testing.T) {
	widA, migA := ownerChain(t, "mismatch0000000001")
	widB, _ := ownerChain(t, "mismatch0000000002")
	_ = widA
	rec := cutoverFinalReqAs(t, widB, migA, "mismatchkey0000001", `{"environment":"dev"}`, "test-admin", "")
	if rec.Code != 404 {
		t.Fatalf("mismatch: got %d %s, want 404", rec.Code, rec.Body.String())
	}
	// Unknown migration is 404 (never accepted).
	rec2 := cutoverFinalReqAs(t, widB, "00000000-0000-0000-0000-000000000000", "mismatchkey0000002", `{"environment":"dev"}`, "test-admin", "")
	if rec2.Code != 404 {
		t.Fatalf("unknown migration: got %d, want 404", rec2.Code)
	}
}

// 7. Unauthorized approval paths: request + decide by a stranger are 403;
// cross-workload approval use is rejected.
func TestAuthUnauthorizedApproval(t *testing.T) {
	wid, _ := ownerChain(t, "unauthappr0000001")
	if rec := postApprovalReqAs(t, wid, "unauthapprkey0001", `{"target_weight":1}`, "test-other", ""); rec.Code != 403 {
		t.Fatalf("stranger request: %d %s", rec.Code, rec.Body.String())
	}
	a := ownerApproval(t, wid, "unauthappr000001")
	// Cross-workload use: approval from widA decided under widB -> 404.
	widB, _ := ownerChain(t, "unauthappr0000002")
	if rec := decideReqAs(t, widB, a.ID, "unauthapprkey0002", "approved", "test-admin", ""); rec.Code != 404 {
		t.Fatalf("cross-workload decide: %d %s", rec.Code, rec.Body.String())
	}
}

// 9. Unauthorized cutover: test-other cannot drive test-owner's migration
// through final cutover (403 before policy/evidence work).
func TestAuthUnauthorizedCutover(t *testing.T) {
	wid, migID := ownerChain(t, "unauthcut00000001")
	rec := cutoverFinalReqAs(t, wid, migID, "unauthcutkey00001", `{"environment":"dev"}`, "test-other", "")
	if rec.Code != 403 {
		t.Fatalf("unauthorized cutover: %d %s", rec.Code, rec.Body.String())
	}
}

// 11. Ownership concurrency: concurrent transfers collapse to one winner;
// duplicates replay safely; reversal is rejected; a stale (non-aws)
// expectation is rejected.
func TestOwnershipConcurrencyOneWinner(t *testing.T) {
	resetStore()
	const n = 16
	wins := 0
	var mu sync.Mutex
	var wg sync.WaitGroup
	mk := func() OwnershipRecord {
		return OwnershipRecord{MigrationID: "m-conc", WorkloadID: "w-conc",
			CurrentOwner: "azure", PreviousOwner: "aws", Routing: "azure"}
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := store.TransferOwnership("m-conc", "aws", mk()); ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("winners=%d, want exactly 1", wins)
	}
	// Duplicate (already azure): rejected.
	if ok, _ := store.TransferOwnership("m-conc", "aws", mk()); ok {
		t.Fatal("duplicate transfer must not win")
	}
	// Reversal (expect azure, record azure target): rejected.
	if ok, _ := store.TransferOwnership("m-conc", "azure",
		OwnershipRecord{MigrationID: "m-conc", WorkloadID: "w-conc", CurrentOwner: "aws", PreviousOwner: "azure"}); ok {
		t.Fatal("reversal must be rejected")
	}
	// Stale expectation (expect azure on absent record): rejected.
	if ok, _ := store.TransferOwnership("m-stale", "azure", mk()); ok {
		t.Fatal("stale expectation must be rejected")
	}
	rec, ok := store.GetOwnership("m-conc")
	if !ok || rec.CurrentOwner != "azure" || rec.PreviousOwner != "aws" {
		t.Fatalf("record: %+v %v", rec, ok)
	}
}

// 12. Process failure during transfer converges safely: with the azure fact
// committed but shops still aws/aws (crash between commit and first flip),
// the next attempt resumes forward to COMPLETE — never dual, never lost.
func TestCutoverCrashBetweenCommitAndFlips(t *testing.T) {
	wid, migID, apprID, _, _, _, restore := cutoverFixture(t, "cutcrash000000001")
	defer restore()
	// Simulate the crash: fact committed, shops untouched.
	now := "2026-01-01T00:00:00Z"
	moved, err := store.TransferOwnership(migID, "aws", OwnershipRecord{
		MigrationID: migID, WorkloadID: wid, CurrentOwner: "azure", PreviousOwner: "aws",
		Routing: "azure", ApprovalID: apprID, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil || !moved {
		t.Fatalf("setup commit: %v %v", moved, err)
	}
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutcrashkey0000001", cutoverBody(apprID)))
	if out["status"] != CutoverComplete {
		t.Fatalf("resume: status=%v failure=%v %v", out["status"], out["failure_code"], out["failure_reason"])
	}
	rec, ok := store.GetOwnership(migID)
	if !ok || rec.CurrentOwner != "azure" {
		t.Fatalf("record: %+v %v", rec, ok)
	}
}

// 13. Audit actor forgery: headers claiming another identity never reach the
// audit trail; the verified bearer principal does.
func TestAuditActorForgery(t *testing.T) {
	resetStore()
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads",
		strings.NewReader(`{"schema_version":1,"name":"cloudshop","compute":{"orchestrator":"kubernetes","replicas":2,"cpu_millicores":500,"memory_mib":512},"database":{"engine":"postgresql","major_version":"17","storage_gib":20},"cache":{"engine":"redis","authoritative":false},"object_storage":{"required":true,"versioning_required":true},"queue":{"delivery_semantics":"at_least_once","duplicate_safe_consumer":true},"requirements":{"rpo_seconds":30,"rto_seconds":900,"private_data_plane":true}}`))
	req.Header.Set("Idempotency-Key", "auditforgekey00001")
	req.Header.Set("Authorization", "Bearer test-token-test-other")
	req.Header.Set("X-Actor-Id", "test-admin")
	req.Header.Set("X-Actor-Type", "human")
	rec := httptest.NewRecorder()
	registerWorkload(rec, req)
	if rec.Code != 201 {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
	}
	ms := store.(*MemStore)
	if len(ms.audits) != 1 {
		t.Fatalf("audits=%d", len(ms.audits))
	}
	au := ms.audits[0]
	if au.ActorID != "test-other" || au.ActorType != "human" {
		t.Fatalf("forged audit identity: %+v", au)
	}
	// Approval request/decide audits likewise bind verified identities.
	var wl map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &wl)
	wid := wl["id"].(string)
	_ = wid // owned by test-other; approvals below use the demo admin pair on their own chain
	resetStore()
	wid2 := approvalChain(t, "auditforge0000001")
	arec := postApprovalReq(t, wid2, "auditforgekey0002", `{"target_weight":1}`, "requester-1", "human")
	if arec.Code != 201 {
		t.Fatalf("approval: %d %s", arec.Code, arec.Body.String())
	}
	a := decodeApproval(t, arec)
	if a.RequestedBy != "requester-1" || a.RequestedByType != "human" {
		t.Fatalf("requested_by forged: %+v", a)
	}
	drec := decideReq(t, wid2, a.ID, "auditforgekey0003", "approved", "approver-1", "human")
	if drec.Code != 200 {
		t.Fatalf("decide: %d %s", drec.Code, drec.Body.String())
	}
	decided := decodeApproval(t, drec)
	if decided.DecidedBy != "approver-1" || decided.DecidedByType != "human" {
		t.Fatalf("decided_by forged: %+v", decided)
	}
	for _, au := range ms.audits {
		if au.Action == "decide_approval" {
			if au.ActorID != "approver-1" || au.ActorType != "human" {
				t.Fatalf("decided_by forged: %+v", au)
			}
		}
	}
}

// 14. Approval + execute with a valid authenticated actor: the allow-path
// chain authorizes end-to-end (approval where required, 202 execute).
func TestAuthValidApprovalExecute(t *testing.T) {
	resetStore()
	wid, migID := execChain(t, "authvalid00000001")
	fake := useFakeClient(t)
	rec := postExecuteReq(t, migID, "authvalidkey000002", `{"target_weight":1}`)
	if rec.Code != 202 {
		t.Fatalf("valid execute: %d %s", rec.Code, rec.Body.String())
	}
	if fake.starts != 1 {
		t.Fatalf("starts=%d", fake.starts)
	}
	m := decodeExecution(t, rec)
	if m["status"] != ExecRunning {
		t.Fatalf("status=%v", m["status"])
	}
	// Gated path: conditional evidence requires approval; owner requests,
	// demo approver decides; execute with approval succeeds.
	resetStore()
	wid2 := approvalChain(t, "authvalid00000002")
	mig2 := createMig(t, wid2)
	a := decodeApproval(t, postApprovalReq(t, wid2, "authvalidkey000003", `{"target_weight":1}`, "requester-1", ""))
	if drec := decideReq(t, wid2, a.ID, "authvalidkey000004", "approved", "approver-1", ""); drec.Code != 200 {
		t.Fatalf("decide: %d %s", drec.Code, drec.Body.String())
	}
	fake2 := useFakeClient(t)
	rec2 := postExecuteReq(t, mig2, "authvalidkey000005", `{"target_weight":1,"approval_id":"`+a.ID+`"}`)
	if rec2.Code != 202 {
		t.Fatalf("approved execute: %d %s", rec2.Code, rec2.Body.String())
	}
	if fake2.starts != 1 {
		t.Fatalf("starts=%d", fake2.starts)
	}
	_ = wid
	_ = wid2
}

// 15. Existing valid local demo path: the demo actor pair (requester-1 +
// approver-1, as used by scripts/demo-migration.sh) still drives the full
// chain to CUTOVER_COMPLETE.
func TestAuthDemoPathEndToEnd(t *testing.T) {
	wid, migID, apprID, _, qsrc, qtgt, restore := cutoverFixture(t, "demopath0000000001")
	defer restore()
	_ = apprID
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "demopathkey0000001", cutoverBody(apprID)))
	if out["status"] != CutoverComplete {
		t.Fatalf("demo path: status=%v failure=%v %v", out["status"], out["failure_code"], out["failure_reason"])
	}
	if qsrc.ownership != "azure" || qtgt.ownership != "azure" {
		t.Fatalf("shops src=%s tgt=%s", qsrc.ownership, qtgt.ownership)
	}
}
