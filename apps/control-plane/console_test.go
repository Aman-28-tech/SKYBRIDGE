// Console read-only aggregation tests: summary derives lifecycle/cutover/
// CDC/safety strictly from stored evidence, never fabricates completion,
// and never mutates (no audit writes, no ownership changes).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func seedConsoleFixture(t *testing.T) (string, string) {
	t.Helper()
	resetStore()
	wid := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaa0001"
	mid := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if err := store.CreateWorkload(wid, map[string]any{
		"id": wid, "name": "cloudshop", "schema_version": 1,
		"lifecycle_state": "registered",
		"canonical_spec": map[string]any{
			"requirements": map[string]any{"rpo_seconds": float64(30)},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateMigration(mid, map[string]any{
		"id": mid, "workload_id": wid, "status": "REGISTERED", "current_step": "registered",
	}); err != nil {
		t.Fatal(err)
	}
	return wid, mid
}

func TestListMigrationsForWorkload(t *testing.T) {
	wid, mid := seedConsoleFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/workloads/"+wid+"/migrations", nil)
	rec := httptest.NewRecorder()
	listMigrationsForWorkload(rec, req, wid)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		WorkloadID string           `json:"workload_id"`
		Items      []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.WorkloadID != wid || len(body.Items) != 1 {
		t.Fatalf("unexpected list: %+v", body)
	}
	if body.Items[0]["id"] != mid {
		t.Fatalf("wrong migration: %+v", body.Items[0])
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/workloads/does-not-exist/migrations", nil)
	rec = httptest.NewRecorder()
	listMigrationsForWorkload(rec, req, "does-not-exist")
	if rec.Code != 404 {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestMigrationSummaryFresh(t *testing.T) {
	_, mid := seedConsoleFixture(t)
	before := len(store.GetAuditForMigration(mid))
	req := httptest.NewRequest(http.MethodGet, "/v1/migrations/"+mid+"/summary", nil)
	rec := httptest.NewRecorder()
	getMigrationSummary(rec, req, mid)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	lc, _ := body["lifecycle"].(map[string]any)
	if lc["current"] != "COMPATIBILITY" {
		t.Fatalf("fresh migration should sit at COMPATIBILITY, got %v", lc["current"])
	}
	stages, _ := lc["stages"].([]any)
	if len(stages) != 8 {
		t.Fatalf("expected 8 lifecycle stages, got %d", len(stages))
	}
	// Only REGISTERED may be completed on a fresh migration.
	for _, s := range stages {
		m, _ := s.(map[string]any)
		if m["name"] == "REGISTERED" && m["status"] != "completed" {
			t.Fatalf("REGISTERED must be completed: %+v", m)
		}
		if m["name"] != "REGISTERED" && m["status"] == "completed" {
			t.Fatalf("must not claim completion without evidence: %+v", m)
		}
	}
	cut, _ := body["cutover"].(map[string]any)
	cstages, _ := cut["stages"].([]any)
	if len(cstages) != 9 {
		t.Fatalf("expected 9 cutover stages, got %d", len(cstages))
	}
	for _, s := range cstages {
		m, _ := s.(map[string]any)
		if m["status"] == "completed" {
			t.Fatalf("fresh cutover must not claim completed: %+v", m)
		}
	}
	cdc, _ := body["cdc"].(map[string]any)
	if cdc["source_lsn"] != "" {
		t.Fatalf("fresh CDC source_lsn must be empty, got %v", cdc["source_lsn"])
	}
	own, _ := body["ownership"].(map[string]any)
	if own["current_owner"] != "aws" || own["split_brain"] != "SAFE" {
		t.Fatalf("fresh ownership must be aws/SAFE: %+v", own)
	}
	if own["source_writable"] != true || own["target_writable"] != false {
		t.Fatalf("fresh writability wrong: %+v", own)
	}
	// Read-only: no audit entries created by the summary.
	if after := len(store.GetAuditForMigration(mid)); after != before {
		t.Fatalf("summary mutated audit trail: before=%d after=%d", before, after)
	}
}

func TestMigrationSummaryFullCutover(t *testing.T) {
	wid, mid := seedConsoleFixture(t)
	if err := store.SaveCompatReport(CompatReport{
		ID: wid + "-compat", WorkloadID: wid, TargetProvider: "azure",
		Status: "conditional",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTargetPlan(TargetMigrationPlan{
		ID: wid + "-plan", WorkloadID: wid, TargetProvider: "azure",
		OverallStatus: "conditional",
	}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []AuditEntry{
		{RunID: mid, WorkloadID: wid, Action: "rehearsal_complete", Result: "success", RequestID: "r1"},
		{RunID: mid, WorkloadID: wid, Action: "set_quiesce", Result: "success", RequestID: "r2", Metadata: `{"quiesced":true}`},
		{RunID: mid, WorkloadID: wid, Action: "final_preflight", Result: "success", RequestID: "r3", PolicyDecision: "allow", Metadata: `{"status":"READY_FOR_CUTOVER","policy_input_hash":"abc"}`},
		{RunID: mid, WorkloadID: wid, Action: "final_quiesce", Result: "success", RequestID: "r4"},
		{RunID: mid, WorkloadID: wid, Action: "transfer_ownership", Result: "success", RequestID: "r5", ApprovalID: "appr-1", Metadata: `{"previous_owner":"aws","current_owner":"azure","source_lsn":"0/A001","applied_lsn":"0/A001","cdc_lag_seconds":2}`},
		{RunID: mid, WorkloadID: wid, Action: "switch_routing", Result: "success", RequestID: "r6", Metadata: `{"routing":"azure"}`},
		{RunID: mid, WorkloadID: wid, Action: "resume_writes", Result: "success", RequestID: "r7"},
		{RunID: mid, WorkloadID: wid, Action: "cutover_complete", Result: "success", RequestID: "r8", Metadata: `{"current_owner":"azure","routing":"azure","source_lsn":"0/A001","applied_lsn":"0/A001"}`},
		{RunID: mid, WorkloadID: wid, Action: "rehearsal_replication", Result: "success", RequestID: "r9", Metadata: `{"source_lsn":"0/A001","applied_lsn":"0/A001","cdc_lag_seconds":2,"events_captured":7,"events_applied":5,"events_duplicates":2}`},
		{RunID: mid, WorkloadID: wid, Action: "rehearsal_validation", Result: "success", RequestID: "r10", Metadata: `{"reconciliation_fingerprint":"fp1","match":true}`},
	} {
		if err := store.RecordAudit(e); err != nil {
			t.Fatal(err)
		}
	}
	// Canary 1/5/25/50 PASS (plus stage 0 baseline).
	for _, stage := range []int{0, 1, 5, 25, 50} {
		if err := store.SaveCanaryRecord(CanaryRecord{
			MigrationID: mid, WorkloadID: wid, Stage: stage, Verdict: CanaryPass,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.TransferOwnership(mid, "aws", OwnershipRecord{
		MigrationID: mid, WorkloadID: wid, CurrentOwner: "azure",
		PreviousOwner: "aws", Routing: "azure",
		SourceLSN: "0/A001", AppliedLSN: "0/A001", LagSeconds: 2,
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/migrations/"+mid+"/summary", nil)
	rec := httptest.NewRecorder()
	getMigrationSummary(rec, req, mid)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	for _, want := range []string{
		`"current":"COMPLETE"`, `"current_owner":"azure"`, `"routing":"azure"`,
		`"source_lsn":"0/A001"`, `"events_captured":7`, `"split_brain":"SAFE"`,
		`"rollback":"POST_WRITE_ROLLBACK_BLOCKED"`,
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("summary lacks %s: %s", want, raw)
		}
	}
	var body struct {
		Cutover struct {
			Status       string `json:"status"`
			CurrentStage string `json:"current_stage"`
			Stages       []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"stages"`
		} `json:"cutover"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Cutover.Status != "CUTOVER_COMPLETE" || body.Cutover.CurrentStage != "CUTOVER_COMPLETE" {
		t.Fatalf("unexpected cutover: %+v", body.Cutover)
	}
	for _, s := range body.Cutover.Stages {
		if s.Status != "completed" {
			t.Fatalf("full cutover stage %s not completed: %+v", s.Name, s)
		}
	}
}

func TestMigrationSummaryNotFound(t *testing.T) {
	resetStore()
	req := httptest.NewRequest(http.MethodGet, "/v1/migrations/does-not-exist/summary", nil)
	rec := httptest.NewRecorder()
	getMigrationSummary(rec, req, "does-not-exist")
	if rec.Code != 404 {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}
