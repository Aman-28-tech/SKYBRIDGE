// H-3 crash-safety tests: durable admin state (file backend).
//
//	process failure during transfer -> safe recovery state: after a
//	  persisted transfer, a simulated restart (zeroed globals reloaded from
//	  the backend) recovers the committed state — never env defaults.
//	persist failure -> mutation refused (503) with memory unchanged, so
//	  memory and persisted facts can never disagree.
//	corrupt persisted state -> load fails closed (never defaulted).
package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// withStateFile points the file backend at a temp path for the test.
func withStateFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin-state.json")
	t.Setenv("CLOUDSHOP_STATE_FILE", path)
	return path
}

// Process failure during transfer recovers the committed state.
func TestAdminStateRestartRecovery(t *testing.T) {
	path := withStateFile(t)
	resetStore()
	defer resetOwnership(t)
	if rec := postOwnership(t, "azure"); rec.Code != 200 {
		t.Fatalf("transfer: %d", rec.Code)
	}
	if rec := setQuiesce(t, true); rec.Code != 200 {
		t.Fatalf("quiesce: %d", rec.Code)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state not persisted: %v", err)
	}
	// Simulate process failure: zero every global, reload from backend.
	quiesceMu.Lock()
	writeOwnership, quiesced = "", false
	quiesceMu.Unlock()
	st, ok, err := loadAdminState()
	if err != nil || !ok {
		t.Fatalf("reload: %+v %v %v", st, ok, err)
	}
	quiesceMu.Lock()
	writeOwnership, quiesced = st.WriteOwnership, st.Quiesced
	quiesceMu.Unlock()
	// Recovered to the committed transfer — never env defaults. The
	// restarted source still rejects writes (azure-named): paused, never a
	// resurrected second writer.
	if currentOwnership() != "azure" || !isQuiesced() {
		t.Fatalf("recovered ownership=%s quiesced=%v", currentOwnership(), isQuiesced())
	}
	t.Setenv("DEPLOYMENT", "aws")
	ownershipOverride = ""
	if rec := doOrder(t, "restartrec0000001", `{"user_id":"u1","total_cents":100}`); rec.Code != 403 {
		t.Fatalf("restarted source must reject writes: %d", rec.Code)
	}
}

// Persist failure refuses the mutation with memory unchanged.
func TestAdminStatePersistFailure(t *testing.T) {
	withStateFile(t)
	resetStore()
	// Point the backend at an unwritable location after reset.
	t.Setenv("CLOUDSHOP_STATE_FILE", filepath.Join(t.TempDir(), "no-such-dir", "state.json"))
	if rec := postOwnership(t, "azure"); rec.Code != 503 {
		t.Fatalf("unwritable backend: got %d, want 503", rec.Code)
	}
	if currentOwnership() != "aws" {
		t.Fatal("failed persist must leave memory unchanged")
	}
	if rec := setQuiesce(t, true); rec.Code != 503 {
		t.Fatalf("unwritable backend quiesce: got %d, want 503", rec.Code)
	}
	if isQuiesced() {
		t.Fatal("failed persist must leave quiesce unchanged")
	}
}

// Corrupt persisted state fails closed.
func TestAdminStateCorrupt(t *testing.T) {
	path := withStateFile(t)
	resetStore()
	if err := os.WriteFile(path, []byte(`{"write_ownership":"gcp"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := loadAdminState(); err == nil || ok {
		t.Fatalf("corrupt state must fail: %+v %v", ok, err)
	}
	if err := os.WriteFile(path, []byte(`not-json`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := loadAdminState(); err == nil || ok {
		t.Fatalf("unparseable state must fail: %+v %v", ok, err)
	}
}

// Unauthenticated admin state probes stay 401 even with a state file.
func TestAdminStateUnauthWithBackend(t *testing.T) {
	withStateFile(t)
	resetStore()
	rec := adminReq(t, http.MethodGet, "/v1/admin/quiesce", "", "")
	if rec.Code != 401 {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}
