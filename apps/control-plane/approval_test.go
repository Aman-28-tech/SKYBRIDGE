// Approval workflow tests: explicit human authorization for gated actions.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func approvalChain(t *testing.T, key string) string {
	t.Helper()
	wid := registerPlanWID(t, key+"reg000001")
	evalCompat(t, wid, key+"cmp000001")
	evalPlanForDrift(t, wid)
	evalDriftForPlan(t, wid, "clean")
	return wid
}

func postApprovalReq(t *testing.T, wid, key, body, actor, actorType string) *httptest.ResponseRecorder {
	return postApprovalReqAs(t, wid, key, body, actor, actorType)
}

func postApprovalReqAs(t *testing.T, wid, key, body, actor, actorType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads/"+wid+"/approvals", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	withBearer(req, actor, actorType)
	rec := httptest.NewRecorder()
	postApproval(rec, req, wid)
	return rec
}

func decideReq(t *testing.T, wid, apprID, key, decision, decider, deciderType string) *httptest.ResponseRecorder {
	return decideReqAs(t, wid, apprID, key, decision, decider, deciderType)
}

func decideReqAs(t *testing.T, wid, apprID, key, decision, decider, deciderType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads/"+wid+"/approvals/"+apprID+"/decision",
		strings.NewReader(`{"decision":"`+decision+`"}`))
	req.Header.Set("Idempotency-Key", key)
	withBearer(req, decider, deciderType)
	rec := httptest.NewRecorder()
	decideApproval(rec, req, wid, apprID)
	return rec
}

func decodeApproval(t *testing.T, rec *httptest.ResponseRecorder) Approval {
	t.Helper()
	var a Approval
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	return a
}

// Create pending approval bound to current evidence.
func TestApprovalCreatePending(t *testing.T) {
	resetStore()
	wid := approvalChain(t, "apprcreate000001")
	ms := store.(*MemStore)
	before := len(ms.audits)
	rec := postApprovalReq(t, wid, "apprcreatekey0001", `{"target_weight":1}`, "requester-1", "")
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	a := decodeApproval(t, rec)
	if a.Decision != ApprovalPending || a.RequestedBy != "requester-1" || a.Risk != RiskMedium {
		t.Fatalf("approval: %+v", a)
	}
	if a.PolicyInputHash == "" || a.PlanID == "" || a.CompatibilityReportID == "" || a.DriftReportID == "" {
		t.Fatalf("bindings missing: %+v", a)
	}
	if exp, _ := parseApprovalTime(a.ExpiresAt); !exp.After(time.Now()) {
		t.Fatal("expiry not in future")
	}
	plan, _ := ms.GetLatestTargetPlan(wid)
	if a.PlanID != plan.ID {
		t.Fatal("plan binding wrong")
	}
	found := false
	for _, au := range ms.audits[len(ms.audits)-(len(ms.audits)-before):] {
		if au.Action == "request_approval" && au.Result == "success" && strings.Contains(au.Metadata, a.ID) {
			found = true
		}
	}
	if !found {
		t.Fatal("request_approval audit missing")
	}
	// replay identical
	rec2 := postApprovalReq(t, wid, "apprcreatekey0001", `{"target_weight":1}`, "requester-1", "")
	if rec2.Code != 201 || rec2.Body.String() != rec.Body.String() {
		t.Fatal("replay broken")
	}
	// conflict
	rec3 := postApprovalReq(t, wid, "apprcreatekey0001", `{"target_weight":5}`, "requester-1", "")
	if rec3.Code != 409 {
		t.Fatalf("expected 409, got %d", rec3.Code)
	}
}

// Authorized approve; unauthorized (self, agent) refused.
func TestApprovalDecide(t *testing.T) {
	resetStore()
	wid := approvalChain(t, "apprdecide000001")
	a := decodeApproval(t, postApprovalReq(t, wid, "apprdecidekey0001", `{"target_weight":1}`, "requester-1", ""))
	ms := store.(*MemStore)
	rec := decideReq(t, wid, a.ID, "apprdecidekey0002", "approved", "requester-1", "")
	if rec.Code != 403 { // self-approval forbidden
		t.Fatalf("self-approve: %d", rec.Code)
	}
	rec = decideReq(t, wid, a.ID, "apprdecidekey0003", "approved", "agent-9", "agent")
	if rec.Code != 403 { // AI cannot approve
		t.Fatalf("agent approve: %d", rec.Code)
	}
	rec = decideReq(t, wid, a.ID, "apprdecidekey0004", "approved", "approver-2", "")
	if rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	decided := decodeApproval(t, rec)
	if decided.Decision != ApprovalApproved || decided.DecidedBy != "approver-2" || decided.DecidedAt == "" {
		t.Fatalf("decided: %+v", decided)
	}
	stored, _ := store.GetApproval(a.ID)
	if stored.Decision != ApprovalApproved {
		t.Fatal("not persisted")
	}
	found := false
	for _, au := range ms.audits {
		if au.Action == "decide_approval" && au.ApprovalID == a.ID && au.Result == "success" {
			found = true
		}
	}
	if !found {
		t.Fatal("decide audit with approval_id missing")
	}
	// second decision settles conflict-free: already decided (authorized
	// actor reaches the settled check; authorization precedes it).
	rec = decideReq(t, wid, a.ID, "apprdecidekey0005", "rejected", "approver-2", "")
	if rec.Code != 409 {
		t.Fatalf("expected 409 settled, got %d", rec.Code)
	}
}

// Denied policy can never become approvable.
func TestApprovalDeniedNeverApprovable(t *testing.T) {
	resetStore()
	wid := approvalChain(t, "apprdeny00000001")
	ms := store.(*MemStore)
	plan, _ := ms.GetLatestTargetPlan(wid)
	_ = store.SaveDriftReport(EvaluateDrift(plan, loadFixture(t, "versioning-off")))
	rec := postApprovalReq(t, wid, "apprdenykey000001", `{"target_weight":1}`, "requester-1", "")
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "POLICY_DENIED") {
		t.Fatalf("deny gateway: %d %s", rec.Code, rec.Body.String())
	}
}

// Use-time binding: exact hash required; weight drift invalidates.
func TestApprovalUseBinding(t *testing.T) {
	resetStore()
	wid := approvalChain(t, "appruse0000000001")
	a := decodeApproval(t, postApprovalReq(t, wid, "apprusekey0000001", `{"target_weight":1}`, "requester-1", ""))
	_ = decideReq(t, wid, a.ID, "apprusekey0000002", "approved", "approver-2", "")
	stored, _ := store.GetApproval(a.ID)
	in, fail := buildPolicyInput(wid, map[string]any{"target_weight": 1}, Principal{ID: "operator", Type: "human"})
	if fail != nil {
		t.Fatal(fail)
	}
	if _, code, _ := approvalEligibleForUse(wid, stored.ID, in); code != "" {
		t.Fatalf("eligible approval rejected: %s", code)
	}
	in5, _ := buildPolicyInput(wid, map[string]any{"target_weight": 5}, Principal{ID: "operator", Type: "human"})
	if _, code, _ := approvalEligibleForUse(wid, stored.ID, in5); code != "APPROVAL_STALE" {
		t.Fatalf("weight change must invalidate: %s", code)
	}
}

// Staleness matrix: plan / compat / drift / bundle changes invalidate.
func TestApprovalStalenessMatrix(t *testing.T) {
	setup := func(t *testing.T) (string, Approval) {
		t.Helper()
		wid := approvalChain(t, "apprstale00000001")
		a := decodeApproval(t, postApprovalReq(t, wid, "apprstalekey00001", `{"target_weight":1}`, "requester-1", ""))
		_ = decideReq(t, wid, a.ID, "apprstalekey00002", "approved", "approver-2", "")
		stored, _ := store.GetApproval(a.ID)
		return wid, stored
	}
	newInput := func(t *testing.T, wid string) PolicyInput {
		t.Helper()
		in, fail := buildPolicyInput(wid, map[string]any{"target_weight": 1}, Principal{ID: "operator", Type: "human"})
		if fail != nil {
			t.Fatal(fail)
		}
		return in
	}
	t.Run("drift-changed", func(t *testing.T) {
		resetStore()
		wid, stored := setup(t)
		ms := store.(*MemStore)
		plan, _ := ms.GetLatestTargetPlan(wid)
		_ = store.SaveDriftReport(EvaluateDrift(plan, loadFixture(t, "versioning-off")))
		if _, code, _ := approvalEligibleForUse(wid, stored.ID, newInput(t, wid)); code != "APPROVAL_STALE" {
			t.Fatalf("drift change must invalidate: %s", code)
		}
	})
	t.Run("plan-changed", func(t *testing.T) {
		resetStore()
		wid, stored := setup(t)
		fresh := newInput(t, wid) // build while evidence is current
		ms := store.(*MemStore)
		plan, _ := ms.GetLatestTargetPlan(wid)
		plan.PlannerVersion = "planner-v9"
		_ = ms.SaveTargetPlan(plan)
		if _, code, _ := approvalEligibleForUse(wid, stored.ID, fresh); code != "APPROVAL_STALE" {
			t.Fatalf("plan change must invalidate: %s", code)
		}
	})
	t.Run("bundle-changed", func(t *testing.T) {
		resetStore()
		wid, stored := setup(t)
		t.Setenv("POLICY_BUNDLE_VERSION", "v9-test")
		if _, code, _ := approvalEligibleForUse(wid, stored.ID, newInput(t, wid)); code != "APPROVAL_STALE" {
			t.Fatalf("bundle change must invalidate: %s", code)
		}
	})
	t.Run("deny-supersedes", func(t *testing.T) {
		// Evidence moved to deny: the bound hash no longer matches, so the
		// approval is stale (refused). Hash covers every decision input, so
		// a deny outcome can never coincide with a matching hash — execution
		// is impossible either way.
		resetStore()
		wid, stored := setup(t)
		ms := store.(*MemStore)
		plan, _ := ms.GetLatestTargetPlan(wid)
		_ = store.SaveDriftReport(EvaluateDrift(plan, loadFixture(t, "network-open")))
		if _, code, _ := approvalEligibleForUse(wid, stored.ID, newInput(t, wid)); code != "APPROVAL_STALE" {
			t.Fatalf("deny evidence must invalidate: %s", code)
		}
		// And the fresh decision itself is deny.
		in := newInput(t, wid)
		if dec, _ := policyDecide(in); dec != PolicyDeny {
			t.Fatalf("fresh decision: %s", dec)
		}
	})
}

// Expired and rejected approvals are unusable.
func TestApprovalExpiryRejected(t *testing.T) {
	resetStore()
	wid := approvalChain(t, "apprexp0000000001")
	a := decodeApproval(t, postApprovalReq(t, wid, "apprexpkey0000001", `{"target_weight":1}`, "requester-1", ""))
	ms := store.(*MemStore)
	stored, _ := ms.GetApproval(a.ID)
	stored.ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	_ = ms.SaveApproval(stored)
	if rec := decideReq(t, wid, a.ID, "apprexpkey0000002", "approved", "approver-2", ""); rec.Code != 409 ||
		!strings.Contains(rec.Body.String(), "APPROVAL_EXPIRED") {
		t.Fatalf("expired decide: %d %s", rec.Code, rec.Body.String())
	}
	// Expire an approved approval: unusable at execution time.
	a2 := decodeApproval(t, postApprovalReq(t, wid, "apprexpkey0000003", `{"target_weight":5}`, "requester-1", ""))
	_ = decideReq(t, wid, a2.ID, "apprexpkey0000004", "approved", "approver-2", "")
	stored2, _ := ms.GetApproval(a2.ID)
	stored2.ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	_ = ms.SaveApproval(stored2)
	in, _ := buildPolicyInput(wid, map[string]any{"target_weight": 5}, Principal{ID: "operator", Type: "human"})
	if _, code, _ := approvalEligibleForUse(wid, a2.ID, in); code != "APPROVAL_EXPIRED" {
		t.Fatalf("expired use: %s", code)
	}
	_ = decideReq(t, wid, a2.ID, "apprexpkey0000005", "rejected", "approver-2", "")
	// Rejected path needs its own approval (a2 is approved-but-expired).
	a3 := decodeApproval(t, postApprovalReq(t, wid, "apprexpkey0000006", `{"target_weight":1}`, "requester-1", ""))
	_ = decideReq(t, wid, a3.ID, "apprexpkey0000007", "rejected", "approver-2", "")
	in3, _ := buildPolicyInput(wid, map[string]any{"target_weight": 1}, Principal{ID: "operator", Type: "human"})
	if _, code, _ := approvalEligibleForUse(wid, a3.ID, in3); code != "APPROVAL_INVALID" {
		t.Fatalf("rejected use: %s", code)
	}
}

// Race: concurrent decisions on one approval -> exactly one winner.
func TestApprovalDecideRace(t *testing.T) {
	resetStore()
	wid := approvalChain(t, "apprrace000000001")
	a := decodeApproval(t, postApprovalReq(t, wid, "apprracekey000001", `{"target_weight":1}`, "requester-1", ""))
	const n = 12
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := decideReq(t, wid, a.ID, "apprracekey1"+string(rune('a'+i))+"0000", "approved", "approver-2", "")
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, c := range codes {
		switch c {
		case 200:
			wins++
		case 409:
		default:
			t.Fatalf("unexpected %d", c)
		}
	}
	if wins != 1 {
		t.Fatalf("winners=%d", wins)
	}
	ms := store.(*MemStore)
	decides := 0
	for _, au := range ms.audits {
		if au.Action == "decide_approval" && au.ApprovalID == a.ID {
			decides++
		}
	}
	if decides != 1 {
		t.Fatalf("decide audits=%d", decides)
	}
}

// Cutover eligibility end-to-end: approval makes the gated stage acceptable,
// recorded with approval_id; without it, still refused.
func TestCutoverWithApproval(t *testing.T) {
	resetStore()
	wid := approvalChain(t, "apprcut0000000001")
	migID := createMig(t, wid)
	a := decodeApproval(t, postApprovalReq(t, wid, "apprcutkey0000001", `{"target_weight":1}`, "requester-1", ""))
	// without approval -> refused
	rec := cutStage(t, migID, `{"target_weight":1}`)
	if rec.Code != 409 {
		t.Fatalf("unapproved gated cutover: %d", rec.Code)
	}
	_ = decideReq(t, wid, a.ID, "apprcutkey0000002", "approved", "approver-2", "")
	// with approval -> accepted, approval_id recorded
	rec = cutStage(t, migID, `{"target_weight":1,"approval_id":"`+a.ID+`"}`)
	if rec.Code != 202 {
		t.Fatalf("approved cutover: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), a.ID) {
		t.Fatalf("approval_id missing from response: %s", rec.Body.String())
	}
	ms := store.(*MemStore)
	found := false
	for _, au := range ms.audits {
		if au.Action == "shift_traffic" && au.ApprovalID == a.ID && au.Result == "success" {
			found = true
		}
	}
	if !found {
		t.Fatal("cutover audit lacks approval_id")
	}
	// deny evidence defeats even a valid approval
	plan, _ := ms.GetLatestTargetPlan(wid)
	_ = store.SaveDriftReport(EvaluateDrift(plan, loadFixture(t, "network-open")))
	rec = cutStage(t, migID, `{"target_weight":1,"approval_id":"`+a.ID+`"}`)
	if rec.Code != 409 {
		t.Fatalf("deny must win: %d", rec.Code)
	}
	if len(ms.migrations) != 1 {
		t.Fatalf("migrations=%d", len(ms.migrations))
	}
}

// List endpoint with status filter.
func TestApprovalList(t *testing.T) {
	resetStore()
	wid := approvalChain(t, "apprlist000000001")
	a := decodeApproval(t, postApprovalReq(t, wid, "apprlistkey000001", `{"target_weight":1}`, "requester-1", ""))
	_ = decideReq(t, wid, a.ID, "apprlistkey000002", "approved", "approver-2", "")
	req := httptest.NewRequest(http.MethodGet, "/v1/workloads/"+wid+"/approvals?status=approved", nil)
	rec := httptest.NewRecorder()
	getApprovalList(rec, req, wid)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), a.ID) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	req2 := httptest.NewRequest(http.MethodGet, "/v1/workloads/"+wid+"/approvals?status=pending", nil)
	rec2 := httptest.NewRecorder()
	getApprovalList(rec2, req2, wid)
	if rec2.Code != 200 || strings.Contains(rec2.Body.String(), a.ID) {
		t.Fatalf("filter: %d %s", rec2.Code, rec2.Body.String())
	}
}
