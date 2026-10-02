// Read-only console endpoints: GET migration record + GET migration audit
// timeline (run: go test ./...). These endpoints serve the Next.js console;
// they never mutate. Unknown IDs are 404 (never an empty-timeline misread).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func seedMigration(t *testing.T, wid, mid string) {
	t.Helper()
	resetStore()
	if err := store.CreateWorkload(wid, map[string]any{"id": wid, "name": "cloudshop"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateMigration(mid, map[string]any{
		"id": mid, "workload_id": wid, "status": "REGISTERED", "current_step": "registered",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGetMigrationRecord(t *testing.T) {
	wid, mid := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	seedMigration(t, wid, mid)

	req := httptest.NewRequest(http.MethodGet, "/v1/migrations/"+mid, nil)
	rec := httptest.NewRecorder()
	getMigration(rec, req, mid)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "workload_id", "status", "current_step"} {
		if body[k] == nil || body[k] == "" {
			t.Fatalf("MigrationRun missing %q: %v", k, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/migrations/does-not-exist", nil)
	rec = httptest.NewRecorder()
	getMigration(rec, req, "does-not-exist")
	if rec.Code != 404 {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestGetMigrationAuditTimeline(t *testing.T) {
	wid, mid := "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"
	other := "55555555-5555-4555-8555-555555555555"
	seedMigration(t, wid, mid)
	if err := store.CreateMigration(other, map[string]any{"id": other}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []AuditEntry{
		{RunID: mid, Action: "rehearsal_replication", Result: "success", RequestID: "req-1"},
		{RunID: other, Action: "noise", Result: "success", RequestID: "req-x"},
		{RunID: mid, Action: "rehearsal_validation", Result: "success", RequestID: "req-2"},
	} {
		if err := store.RecordAudit(e); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/migrations/"+mid+"/audit", nil)
	rec := httptest.NewRecorder()
	getMigrationAudit(rec, req, mid)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		MigrationID string       `json:"migration_id"`
		Items       []AuditEntry `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.MigrationID != mid {
		t.Fatalf("wrong migration: %s", body.MigrationID)
	}
	if len(body.Items) != 2 {
		t.Fatalf("expected 2 scoped entries, got %d", len(body.Items))
	}
	if body.Items[0].Action != "rehearsal_replication" || body.Items[1].Action != "rehearsal_validation" {
		t.Fatalf("not chronological: %+v", body.Items)
	}
	for _, it := range body.Items {
		if it.CreatedAt == "" {
			t.Fatalf("timeline entry lacks created_at: %+v", it)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/migrations/does-not-exist/audit", nil)
	rec = httptest.NewRecorder()
	getMigrationAudit(rec, req, "does-not-exist")
	if rec.Code != 404 {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestGetMigrationAuditEmptyIsNotMisleading(t *testing.T) {
	wid, mid := "66666666-6666-4666-8666-666666666666", "77777777-7777-4777-8777-777777777777"
	seedMigration(t, wid, mid)
	req := httptest.NewRequest(http.MethodGet, "/v1/migrations/"+mid+"/audit", nil)
	rec := httptest.NewRecorder()
	getMigrationAudit(rec, req, mid)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("expected explicit empty items, got %s", rec.Body.String())
	}
}

// Replication audit metadata must carry the measured event counts so the
// console CDC view reads real values (never fabricated history).
func TestRehearsalReplicationAuditCounts(t *testing.T) {
	wid, migID := rehearsalChain(t, "consoleaudit0001")
	restore := useFake(&fakeRehearsalDataPlane{
		Report: CDCReport{
			SourceLSN: "0/A001", AppliedLSN: "0/A001", BaseLSN: "0/A000",
			ProbeIDs: []string{"probe-1"}, LagSeconds: 5, WithinRPO: true,
			EventsCaptured: 7, EventsApplied: 5, EventsDuplicates: 2,
			TargetHealthy: true,
		},
		Recon: cleanRecon(1),
	})
	defer restore()
	rec := rehearseReq(t, wid, migID, "consoleauditkey01", rehearsalBody)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	found := false
	for _, a := range store.GetAuditForMigration(migID) {
		if a.Action != "rehearsal_replication" {
			continue
		}
		found = true
		for _, want := range []string{
			`"events_captured":7`, `"events_applied":5`, `"events_duplicates":2`,
			`"source_lsn":"0/A001"`, `"cdc_lag_seconds":5`,
		} {
			if !strings.Contains(a.Metadata, want) {
				t.Fatalf("replication metadata lacks %s: %s", want, a.Metadata)
			}
		}
	}
	if !found {
		t.Fatal("no rehearsal_replication audit entry")
	}
}
