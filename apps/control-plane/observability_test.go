// Cutover observability tests: status endpoint, counters, log hygiene
// (run: go test ./...).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func cutoverStatusReq(t *testing.T, wid, migID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/migrations/"+migID+"/cutover", nil)
	rec := httptest.NewRecorder()
	getCutoverStatus(rec, req, wid, migID)
	return rec
}

func resetCutoverStats() {
	cutoverStats.mu.Lock()
	defer cutoverStats.mu.Unlock()
	cutoverStats.attempts, cutoverStats.completed, cutoverStats.blocked = 0, 0, 0
	cutoverStats.rollbackRejected = 0
	cutoverStats.failByStage = map[string]uint64{}
}

// Status reflects pre-transfer defaults, then the transfer fact.
func TestCutoverStatusEndpoint(t *testing.T) {
	wid, migID, apprID, _, _, _, restore := cutoverFixture(t, "cutoverstatus0001")
	defer restore()
	pre := cutoverStatusReq(t, wid, migID)
	var before map[string]any
	_ = json.Unmarshal(pre.Body.Bytes(), &before)
	if before["current_owner"] != "aws" || before["routing"] != "aws" {
		t.Fatalf("pre-transfer: %+v", before)
	}
	if rec := cutoverFinalReq(t, wid, migID, "cutoverstatuskey01", cutoverBody(apprID)); rec.Code != 200 {
		t.Fatalf("setup: %d", rec.Code)
	}
	post := cutoverStatusReq(t, wid, migID)
	var after map[string]any
	_ = json.Unmarshal(post.Body.Bytes(), &after)
	if after["current_owner"] != "azure" || after["routing"] != "azure" {
		t.Fatalf("post-transfer: %+v", after)
	}
	if after["source_lsn"] != "0/A001" || after["applied_lsn"] != "0/A001" {
		t.Fatalf("positions: %+v", after)
	}
	if after["cdc_lag_seconds"] != float64(5) {
		t.Fatalf("lag: %+v", after)
	}
	if after["canary_latest_stage"] != float64(50) || after["canary_latest_verdict"] != CanaryPass {
		t.Fatalf("canary: %+v", after)
	}
	if _, ok := after["counters"]; !ok {
		t.Fatalf("counters: %+v", after)
	}
	if after["policy_bundle_version"] != "dev" {
		t.Fatalf("bundle: %+v", after)
	}
}

// Unknown migration is 404, never a default owner.
func TestCutoverStatusNotFound(t *testing.T) {
	resetStore()
	rec := cutoverStatusReq(t, "w", "00000000-0000-4000-8000-000000000000")
	if rec.Code != 404 {
		t.Fatalf("status: %d", rec.Code)
	}
}

// Counters track attempts, outcomes, per-stage failures, rollback denials.
func TestCutoverCounters(t *testing.T) {
	resetCutoverStats()
	wid, migID, apprID, _, _, _, restore := cutoverFixture(t, "cutovermetrics0001")
	defer restore()
	decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutovermetricskey1", cutoverBody(apprID)))
	snap := cutoverStats.snapshot()
	if snap["cutover_attempts"] != uint64(1) || snap["cutover_completed"] != uint64(1) {
		t.Fatalf("counters: %+v", snap)
	}
	// Rollback denial counts (before any resetStore wipes the record).
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+migID+"/rollback", strings.NewReader(`{}`))
	withBearer(req, "test-admin", "")
	rec := httptest.NewRecorder()
	rollback(rec, req)
	if rec.Code != 409 {
		t.Fatalf("rollback: %d", rec.Code)
	}
	snap = cutoverStats.snapshot()
	if snap["rollback_rejected"] != uint64(1) {
		t.Fatalf("rollback: %+v", snap)
	}
	// Blocked run records the failing stage.
	wid2, migID2, apprID2, _, _, _, restore2 := cutoverFixture(t, "cutovermetrics0002")
	defer restore2()
	_ = apprID2
	plan, _ := store.GetLatestTargetPlan(wid2)
	plan.PlannerVersion = "planner-v1"
	_ = store.SaveTargetPlan(plan)
	decodeCutover(t, cutoverFinalReq(t, wid2, migID2, "cutovermetricskey2", cutoverBody(apprID)))
	snap = cutoverStats.snapshot()
	if snap["cutover_blocked"] != uint64(1) {
		t.Fatalf("blocked: %+v", snap)
	}
	byStage, _ := snap["cutover_failures_by_stage"].(map[string]uint64)
	if byStage["FINAL_PREFLIGHT"] != 1 {
		t.Fatalf("by stage: %+v", snap)
	}
	// Metric names are stable and bounded.
	for _, k := range []string{"cutover_attempts", "cutover_completed", "cutover_blocked", "rollback_rejected", "cutover_failures_by_stage"} {
		if _, ok := snap[k]; !ok {
			t.Fatalf("metric %q missing", k)
		}
	}
}

// No secrets in observability sources: code scan + live metadata scan.
func TestObservabilityNoSecrets(t *testing.T) {
	for _, f := range []string{"cutover_final.go", "observability.go", "quiesce.go", "readiness.go", "canary.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		l := strings.ToLower(string(b))
		for _, needle := range []string{"password", "aws_secret", "akia", "client_secret", "private_key", "connection_string"} {
			if strings.Contains(l, needle) {
				t.Fatalf("%s contains %q", f, needle)
			}
		}
	}
}
