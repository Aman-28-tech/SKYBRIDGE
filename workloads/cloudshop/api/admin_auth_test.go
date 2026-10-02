// H-3 targeted tests: authenticated admin surface.
//
//	unauthenticated admin request -> 401 (GET and POST, quiesce and ownership)
//	authenticated unauthorized actor -> 403 (POST with non-admin token)
//	authorized actor -> allowed (GET with any valid token, POST with admin)
//	forged actor headers -> 401 (no bearer confers nothing)
//	empty registry -> 401 even for well-formed bearers
//	concurrent transfers -> converged single state, never an error
//	duplicate transfer -> replayed safely (same state, 200)
//
// Reversal/stale-worker/duplication-at-the-record rejections live at the
// control-plane CAS (TransferOwnership: exactly-once aws->azure); the shop
// admin stays bidirectional so deterministic reset (reset-demo.sh) keeps
// working. See docs/SECURITY.md (ownership protection).
//
// Test doubles, NOT secrets: fixture tokens below are in-process test values
// only. Production code defines no default tokens.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// installTestAdminFixtures installs deterministic admin-surface fixtures:
// shop-admin (admin) and shop-user (authenticated, non-admin).
func installTestAdminFixtures() {
	setTestAdminTokens([]adminTokenEntry{
		{Token: "test-shop-admin-0001", ActorID: "shop-admin", Admin: true},
		{Token: "test-shop-user-0001", ActorID: "shop-user", Admin: false},
	})
}

func adminReq(t *testing.T, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	switch path {
	case "/v1/admin/quiesce":
		quiesceAdmin(rec, req)
	case "/v1/admin/ownership":
		ownershipAdmin(rec, req)
	default:
		t.Fatalf("unknown admin path %s", path)
	}
	return rec
}

// Unauthenticated admin access is removed: every combination answers 401.
func TestAdminUnauthenticated(t *testing.T) {
	resetStore()
	cases := []struct{ method, path, body string }{
		{http.MethodGet, "/v1/admin/quiesce", ""},
		{http.MethodPost, "/v1/admin/quiesce", `{"quiesced":true}`},
		{http.MethodGet, "/v1/admin/ownership", ""},
		{http.MethodPost, "/v1/admin/ownership", `{"write_ownership":"azure"}`},
	}
	for _, c := range cases {
		rec := adminReq(t, c.method, c.path, c.body, "")
		if rec.Code != 401 || !strings.Contains(rec.Body.String(), "UNAUTHENTICATED") {
			t.Fatalf("%s %s: got %d %s, want 401", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	// Garbage bearer and forged actor headers confer nothing.
	rec := adminReq(t, http.MethodPost, "/v1/admin/ownership", `{"write_ownership":"azure"}`, "not-a-token")
	if rec.Code != 401 {
		t.Fatalf("garbage bearer: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/ownership", strings.NewReader(`{"write_ownership":"azure"}`))
	req.Header.Set("X-Actor-Id", "shop-admin")
	rec2 := httptest.NewRecorder()
	ownershipAdmin(rec2, req)
	if rec2.Code != 401 {
		t.Fatalf("header-only identity: %d", rec2.Code)
	}
	if currentOwnership() != "aws" {
		t.Fatal("rejected admin mutation must not change ownership")
	}
}

// Authenticated but unauthorized: non-admin reads state (200) but cannot
// mutate (403); state unchanged either way.
func TestAdminUnauthorizedActor(t *testing.T) {
	resetStore()
	if rec := adminReq(t, http.MethodGet, "/v1/admin/quiesce", "", "test-shop-user-0001"); rec.Code != 200 {
		t.Fatalf("non-admin read: %d", rec.Code)
	}
	if rec := adminReq(t, http.MethodGet, "/v1/admin/ownership", "", "test-shop-user-0001"); rec.Code != 200 {
		t.Fatalf("non-admin read: %d", rec.Code)
	}
	for _, tc := range []struct{ path, body string }{
		{"/v1/admin/quiesce", `{"quiesced":true}`},
		{"/v1/admin/ownership", `{"write_ownership":"azure"}`},
	} {
		rec := adminReq(t, http.MethodPost, tc.path, tc.body, "test-shop-user-0001")
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "FORBIDDEN") {
			t.Fatalf("POST %s: got %d %s, want 403", tc.path, rec.Code, rec.Body.String())
		}
	}
	if currentOwnership() != "aws" || isQuiesced() {
		t.Fatal("unauthorized mutation must not change admin state")
	}
}

// Authorized actor: admin reads and mutates; existing CAS-style idempotency
// (same state -> same 200) preserved.
func TestAdminAuthorized(t *testing.T) {
	resetStore()
	defer resetOwnership(t)
	if rec := adminReq(t, http.MethodPost, "/v1/admin/quiesce", `{"quiesced":true}`, "test-shop-admin-0001"); rec.Code != 200 {
		t.Fatalf("quiesce: %d %s", rec.Code, rec.Body.String())
	}
	if rec := adminReq(t, http.MethodPost, "/v1/admin/ownership", `{"write_ownership":"azure"}`, "test-shop-admin-0001"); rec.Code != 200 {
		t.Fatalf("ownership: %d %s", rec.Code, rec.Body.String())
	}
	// Duplicate transfer replays safely: same state, 200.
	rec := adminReq(t, http.MethodPost, "/v1/admin/ownership", `{"write_ownership":"azure"}`, "test-shop-admin-0001")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"azure"`) {
		t.Fatalf("duplicate: %d %s", rec.Code, rec.Body.String())
	}
	// Invalid values still rejected; state unchanged.
	before := currentOwnership()
	if rec := adminReq(t, http.MethodPost, "/v1/admin/ownership", `{"write_ownership":"gcp"}`, "test-shop-admin-0001"); rec.Code != 400 {
		t.Fatalf("invalid owner: %d", rec.Code)
	}
	if currentOwnership() != before {
		t.Fatal("rejected transfer must not mutate ownership")
	}
}

// Empty registry fails closed: no token validates.
func TestAdminEmptyRegistry(t *testing.T) {
	resetStore()
	setTestAdminTokens(nil)
	if rec := adminReq(t, http.MethodGet, "/v1/admin/quiesce", "", "test-shop-admin-0001"); rec.Code != 401 {
		t.Fatalf("empty registry read: %d", rec.Code)
	}
	if rec := adminReq(t, http.MethodPost, "/v1/admin/ownership", `{"write_ownership":"azure"}`, "test-shop-admin-0001"); rec.Code != 401 {
		t.Fatalf("empty registry write: %d", rec.Code)
	}
}

// Concurrent admin transfers converge on one state without errors.
func TestAdminConcurrentTransfer(t *testing.T) {
	resetStore()
	defer resetOwnership(t)
	const n = 16
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = adminReq(t, http.MethodPost, "/v1/admin/ownership",
				`{"write_ownership":"azure"}`, "test-shop-admin-0001").Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("transfer %d: %d", i, c)
		}
	}
	if currentOwnership() != "azure" {
		t.Fatalf("ownership=%s", currentOwnership())
	}
	var out struct {
		WriteOwnership string `json:"write_ownership"`
	}
	rec := adminReq(t, http.MethodGet, "/v1/admin/ownership", "", "test-shop-admin-0001")
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.WriteOwnership != "azure" {
		t.Fatalf("readback: %v %s", err, rec.Body.String())
	}
}
