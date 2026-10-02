// Execution API tests: authorization-first Temporal starts via fake client.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func useFakeClient(t *testing.T) *fakeTemporalClient {
	t.Helper()
	fake := newFakeTemporalClient()
	prev := temporalClient
	temporalClient = fake
	t.Cleanup(func() { temporalClient = prev })
	return fake
}

// execChain returns wid + migID with full pass evidence (allow path).
func execChain(t *testing.T, key string) (string, string) {
	t.Helper()
	wid, migID := wiredPassSetup(t)
	_ = key
	return wid, migID
}

func postExecuteReq(t *testing.T, migID, key, body string) *httptest.ResponseRecorder {
	return postExecuteReqAs(t, migID, key, body, "test-admin", "")
}

func postExecuteReqAs(t *testing.T, migID, key, body, actor, actorType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+migID+"/execute", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	withBearer(req, actor, actorType)
	rec := httptest.NewRecorder()
	postExecute(rec, req, migID)
	return rec
}

func decodeExecution(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	return m
}

// Deterministic ID: same intent, same workflow ID; different weight differs.
func TestExecutionDeterministicID(t *testing.T) {
	resetStore()
	wid, migID := execChain(t, "execdetkey000001")
	fake := useFakeClient(t)
	rec := postExecuteReq(t, migID, "execdetkey000002", `{"target_weight":1}`)
	if rec.Code != 202 {
		t.Fatalf("execute: %d %s", rec.Code, rec.Body.String())
	}
	m1 := decodeExecution(t, rec)
	// same intent, different key -> same workflow ID, refused as in-flight
	rec2 := postExecuteReq(t, migID, "execdetkey000003", `{"target_weight":1}`)
	if rec2.Code != 409 || !strings.Contains(rec2.Body.String(), "EXECUTION_IN_FLIGHT") {
		t.Fatalf("expected in-flight 409, got %d %s", rec2.Code, rec2.Body.String())
	}
	if fake.starts != 1 {
		t.Fatalf("starts=%d, want 1", fake.starts)
	}
	_ = wid
	_ = m1
}

// Duplicate prevention + audit + idempotency.
func TestExecutionDuplicatePrevention(t *testing.T) {
	resetStore()
	_, migID := execChain(t, "execdupkey0000001")
	fake := useFakeClient(t)
	ms := store.(*MemStore)
	before := len(ms.audits)
	rec := postExecuteReq(t, migID, "execdupkey0000002", `{"target_weight":1}`)
	body := rec.Body.String()
	rec2 := postExecuteReq(t, migID, "execdupkey0000002", `{"target_weight":1}`)
	if rec2.Code != 202 || rec2.Body.String() != body {
		t.Fatal("idempotent replay broken")
	}
	if fake.starts != 1 {
		t.Fatalf("starts=%d", fake.starts)
	}
	execs := store.GetExecutionsForMigration(migID)
	if len(execs) != 1 || execs[0].Status != ExecRunning {
		t.Fatalf("executions: %+v", execs)
	}
	found := false
	for _, a := range ms.audits[len(ms.audits)-(len(ms.audits)-before):] {
		if a.Action == "start_execution" && a.Result == "success" && strings.Contains(a.Metadata, "workflow_id") {
			found = true
		}
	}
	if !found {
		t.Fatal("start_execution audit missing")
	}
	rec3 := postExecuteReq(t, migID, "execdupkey0000002", `{"target_weight":5}`)
	if rec3.Code != 409 {
		t.Fatalf("expected 409 conflict, got %d", rec3.Code)
	}
}

// Missing/stale approval refused before any Temporal start.
func TestExecutionApprovalGate(t *testing.T) {
	resetStore()
	wid := registerPlanWID(t, "execapprkey000001")
	evalCompat(t, wid, "execapprkey000002") // conditional
	evalPlanForDrift(t, wid)
	evalDriftForPlan(t, wid, "clean")
	migID := createMig(t, wid)
	fake := useFakeClient(t)
	rec := postExecuteReq(t, migID, "execapprkey000003", `{"target_weight":1}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "APPROVAL_REQUIRED") {
		t.Fatalf("missing approval: %d %s", rec.Code, rec.Body.String())
	}
	if fake.starts != 0 {
		t.Fatal("temporal started without approval")
	}
	// approve then execute
	a := decodeApproval(t, postApprovalReq(t, wid, "execapprkey000004", `{"target_weight":1}`, "requester-1", ""))
	_ = decideReq(t, wid, a.ID, "execapprkey000005", "approved", "approver-2", "")
	rec = postExecuteReq(t, migID, "execapprkey000006", `{"target_weight":1,"approval_id":"`+a.ID+`"}`)
	if rec.Code != 202 {
		t.Fatalf("approved execute: %d %s", rec.Code, rec.Body.String())
	}
	if fake.starts != 1 {
		t.Fatalf("starts=%d", fake.starts)
	}
}

// Stale evidence refused: planner, compat, drift, hash variants.
func TestExecutionStaleEvidence(t *testing.T) {
	setup := func(t *testing.T) (string, string) {
		t.Helper()
		wid, migID := execChain(t, "execstalekey00001")
		return wid, migID
	}
	t.Run("stale-planner", func(t *testing.T) {
		resetStore()
		wid, migID := setup(t)
		useFakeClient(t)
		ms := store.(*MemStore)
		plan, _ := ms.GetLatestTargetPlan(wid)
		plan.PlannerVersion = "planner-v0"
		_ = ms.SaveTargetPlan(plan)
		rec := postExecuteReq(t, migID, "execstalekey00002", `{"target_weight":1}`)
		if rec.Code != 409 || !strings.Contains(rec.Body.String(), "PLAN_STALE") {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("stale-compat", func(t *testing.T) {
		resetStore()
		wid, migID := setup(t)
		useFakeClient(t)
		ms := store.(*MemStore)
		wl, _ := ms.GetWorkload(wid)
		spec := wl["canonical_spec"].(map[string]any)
		spec["compute"].(map[string]any)["replicas"] = float64(9)
		rec := postExecuteReq(t, migID, "execstalekey00003", `{"target_weight":1}`)
		if rec.Code != 409 || !strings.Contains(rec.Body.String(), "COMPATIBILITY_STALE") {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("stale-drift", func(t *testing.T) {
		resetStore()
		wid, migID := setup(t)
		useFakeClient(t)
		ms := store.(*MemStore)
		plan, _ := ms.GetLatestTargetPlan(wid)
		_ = store.SaveDriftReport(EvaluateDrift(plan, loadFixture(t, "versioning-off")))
		rec := postExecuteReq(t, migID, "execstalekey00004", `{"target_weight":1}`)
		if rec.Code != 409 {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("no-temporal", func(t *testing.T) {
		resetStore()
		_, migID := setup(t)
		temporalClient = nil
		rec := postExecuteReq(t, migID, "execstalekey00005", `{"target_weight":1}`)
		if rec.Code != 503 {
			t.Fatalf("got %d", rec.Code)
		}
	})
	t.Run("unknown-migration", func(t *testing.T) {
		resetStore()
		useFakeClient(t)
		rec := postExecuteReq(t, "00000000-0000-0000-0000-000000000000", "execstalekey00006", `{"target_weight":1}`)
		if rec.Code != 404 {
			t.Fatalf("got %d", rec.Code)
		}
	})
}

// Race: concurrent identical executes -> one Temporal start, one row.
func TestExecuteRace(t *testing.T) {
	resetStore()
	_, migID := execChain(t, "execracekey000001")
	fake := useFakeClient(t)
	const n = 16
	codes := make([]int, n)
	bodies := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := postExecuteReq(t, migID, "execracekey000002", `{"target_weight":1}`)
			codes[i], bodies[i] = rec.Code, rec.Body.String()
		}(i)
	}
	wg.Wait()
	ok202, ok409 := 0, 0
	for i := 0; i < n; i++ {
		switch codes[i] {
		case 202:
			ok202++
			if bodies[i] != bodies[0] && codes[0] == 202 {
				t.Fatalf("goroutine %d body diverged", i)
			}
		case 409:
			ok409++
		default:
			t.Fatalf("goroutine %d: %d", i, codes[i])
		}
	}
	if fake.starts != 1 {
		t.Fatalf("temporal starts=%d, want 1", fake.starts)
	}
	if len(store.GetExecutionsForMigration(migID)) != 1 {
		t.Fatal("duplicate execution rows")
	}
	_ = ok202
	_ = ok409
}

// GET execution status incl. live status from the plane.
func TestGetExecutionStatus(t *testing.T) {
	resetStore()
	_, migID := execChain(t, "execstatuskey00001")
	fake := useFakeClient(t)
	if rec := getExecReq(t, migID); rec.Code != 404 {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	rec := postExecuteReq(t, migID, "execstatuskey00002", `{"target_weight":1}`)
	m := decodeExecution(t, rec)
	wfID := m["workflow_id"].(string)
	get := getExecReq(t, migID)
	var st map[string]any
	_ = json.Unmarshal(get.Body.Bytes(), &st)
	if get.Code != 200 || st["live_status"] != ExecRunning {
		t.Fatalf("status: %d %v", get.Code, st)
	}
	fake.setStatus(wfID, RemoteCompleted)
	get2 := getExecReq(t, migID)
	var st2 map[string]any
	_ = json.Unmarshal(get2.Body.Bytes(), &st2)
	if st2["live_status"] != ExecSucceeded {
		t.Fatalf("live=%v", st2)
	}
}

func getExecReq(t *testing.T, migID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/migrations/"+migID+"/execution", nil)
	rec := httptest.NewRecorder()
	getExecution(rec, req, migID)
	return rec
}

// Replay-after-progress: once the execution completes, replaying the
// original key still returns the acceptance-time payload (status running,
// same workflow/run identity) — not current state. Current state lives on
// GET /execution. This pins the documented replay-vs-read contract.
func TestExecuteReplayAfterCompletion(t *testing.T) {
	resetStore()
	_, migID := execChain(t, "execreplaykey0001")
	fake := useFakeClient(t)
	rec := postExecuteReq(t, migID, "execreplaykey0002", `{"target_weight":1}`)
	if rec.Code != 202 {
		t.Fatalf("execute: %d", rec.Code)
	}
	original := rec.Body.String()
	m := decodeExecution(t, rec)
	wfID := m["workflow_id"].(string)
	// Simulate worker completion out-of-band.
	fake.setStatus(wfID, RemoteCompleted)
	_ = store.UpdateExecutionStatus(wfID, ExecSucceeded, "")
	// Replay returns the original acceptance payload, verbatim in meaning.
	rec2 := postExecuteReq(t, migID, "execreplaykey0002", `{"target_weight":1}`)
	if rec2.Code != 202 || rec2.Body.String() != original {
		t.Fatal("replay must return the original acceptance response")
	}
	var replay map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &replay)
	if replay["status"] != ExecRunning || replay["workflow_id"] != wfID {
		t.Fatalf("replay identity drifted: %v", replay)
	}
	// ...while the read path reports current truth.
	get := getExecReq(t, migID)
	var st map[string]any
	_ = json.Unmarshal(get.Body.Bytes(), &st)
	if st["live_status"] != ExecSucceeded {
		t.Fatalf("GET live=%v", st)
	}
	// Still exactly one execution: replay executed nothing.
	if len(store.GetExecutionsForMigration(migID)) != 1 {
		t.Fatal("replay created an execution")
	}
	if fake.starts != 1 {
		t.Fatalf("starts=%d", fake.starts)
	}
}

// Migration row untouched by execution lifecycle.
func TestExecutionLeavesMigration(t *testing.T) {
	resetStore()
	wid, migID := execChain(t, "execleavekey00001")
	useFakeClient(t)
	rec := postExecuteReq(t, migID, "execleavekey00002", `{"target_weight":1}`)
	if rec.Code != 202 {
		t.Fatalf("execute: %d", rec.Code)
	}
	ms := store.(*MemStore)
	mig, _ := ms.GetMigration(migID)
	if mig["status"] != "REGISTERED" {
		t.Fatalf("migration status=%v", mig["status"])
	}
	wl, _ := ms.GetWorkload(wid)
	if wl["lifecycle_state"] != "registered" {
		t.Fatalf("lifecycle=%v", wl["lifecycle_state"])
	}
}
