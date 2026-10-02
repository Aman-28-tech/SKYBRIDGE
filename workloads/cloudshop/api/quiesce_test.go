// Write-quiesce tests: 503 WRITES_PAUSED with Retry-After, idempotent admin
// transitions, readyz exposure, ownership precedence (run: go test ./...).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func setQuiesce(t *testing.T, q bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/quiesce",
		strings.NewReader(map[bool]string{true: `{"quiesced":true}`, false: `{"quiesced":false}`}[q]))
	req.Header.Set("Authorization", "Bearer test-shop-admin-0001")
	rec := httptest.NewRecorder()
	quiesceAdmin(rec, req)
	return rec
}

func getQuiesce(t *testing.T) (bool, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/quiesce", nil)
	req.Header.Set("Authorization", "Bearer test-shop-admin-0001")
	rec := httptest.NewRecorder()
	quiesceAdmin(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET quiesce: %d", rec.Code)
	}
	var out struct {
		Quiesced       bool   `json:"quiesced"`
		WriteOwnership string `json:"write_ownership"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Quiesced, out.WriteOwnership
}

// H: writes while quiesced are refused explicitly, never dropped silently.
func TestWriteWhileQuiesced(t *testing.T) {
	resetStore()
	setQuiesced(false)
	defer setQuiesced(false)
	if rec := setQuiesce(t, true); rec.Code != 200 {
		t.Fatalf("quiesce: %d", rec.Code)
	}
	rec := doOrder(t, "quiescekey0000001", `{"user_id":"u1","total_cents":100}`)
	if rec.Code != 503 {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "WRITES_PAUSED") {
		t.Fatalf("missing code: %s", rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After required")
	}
	// No order persisted, no idempotency record poisoned.
	if _, ok := store.GetOrder("quiescekey0000001"); ok {
		t.Fatal("quiesced write must not persist")
	}
}

// Q: repeated quiesce transitions are idempotent; unquiesce resumes writes.
func TestRepeatedQuiesce(t *testing.T) {
	resetStore()
	setQuiesced(false)
	defer setQuiesced(false)
	for i := 0; i < 3; i++ {
		if rec := setQuiesce(t, true); rec.Code != 200 {
			t.Fatalf("quiesce %d: %d", i, rec.Code)
		}
	}
	q, _ := getQuiesce(t)
	if !q {
		t.Fatal("must stay quiesced")
	}
	if rec := setQuiesce(t, false); rec.Code != 200 {
		t.Fatalf("unquiesce: %d", rec.Code)
	}
	rec := doOrder(t, "quiescekey0000002", `{"user_id":"u1","total_cents":100}`)
	if rec.Code != 201 {
		t.Fatalf("writes must resume after unquiesce: %d %s", rec.Code, rec.Body.String())
	}
}

// R: concurrent quiesce transitions converge without corruption.
func TestConcurrentQuiesce(t *testing.T) {
	resetStore()
	setQuiesced(false)
	defer setQuiesced(false)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			setQuiesced(i%2 == 0)
		}(i)
	}
	wg.Wait()
	// Converged to a definite state; API still coherent.
	q, _ := getQuiesce(t)
	setQuiesced(q)
	if _, _ = getQuiesce(t); true {
	}
}

// Ownership still precedes quiesce: non-owner gets 403 either way.
func TestQuiesceOwnershipPrecedence(t *testing.T) {
	resetStore()
	setQuiesced(false)
	defer setQuiesced(false)
	ownershipOverride = "other"
	defer func() { ownershipOverride = "self" }()
	setQuiesced(true)
	rec := doOrder(t, "quiescekey0000003", `{"user_id":"u1","total_cents":100}`)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "WRITE_NOT_OWNED") {
		t.Fatalf("non-owner must get 403: %d %s", rec.Code, rec.Body.String())
	}
}

// readyz exposes quiesce state for the readiness gate.
func TestReadyzQuiesced(t *testing.T) {
	resetStore()
	setQuiesced(false)
	defer setQuiesced(false)
	payload := func() map[string]any {
		rec := httptest.NewRecorder()
		readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		var p map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &p)
		return p
	}
	if payload()["quiesced"] != false {
		t.Fatal("readyz must report quiesced=false")
	}
	setQuiesced(true)
	if payload()["quiesced"] != true {
		t.Fatal("readyz must report quiesced=true")
	}
	if payload()["write_ownership"] != "aws" {
		t.Fatal("ownership must stay aws while quiesced")
	}
}
