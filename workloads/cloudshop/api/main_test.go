// Unit tests for CloudShop API semantics (run: go test ./...).
// Covers: idempotency replay/conflict, WRITE_NOT_OWNED guard,
// readyz payload, lazy-warm cache HIT/MISS.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func resetStore() {
	store = NewMemStore()
	productCache.Lock()
	productCache.m = map[string]cacheEntry{}
	productCache.Unlock()
	ownershipOverride = "self"
	installTestAdminFixtures()
	// Route through the durable setters so memory and any configured
	// backend (file/PG) stay converged even across test resets.
	_, _, _ = setOwnership("aws")
	_, _ = setQuiesced(false)
	seed()
}

func doOrder(t *testing.T, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	createOrder(rec, req)
	return rec
}

func TestIdempotentReplay(t *testing.T) {
	resetStore()
	body := `{"user_id":"u1","total_cents":100}`
	r1 := doOrder(t, "0123456789abcdef", body)
	r2 := doOrder(t, "0123456789abcdef", body)
	if r1.Code != 201 || r2.Code != 201 {
		t.Fatalf("expected 201/201, got %d/%d", r1.Code, r2.Code)
	}
	if r1.Body.String() != r2.Body.String() {
		t.Fatalf("replay mismatch:\n%s\n%s", r1.Body.String(), r2.Body.String())
	}
}

func TestIdempotencyConflict(t *testing.T) {
	resetStore()
	r1 := doOrder(t, "0123456789abcdef", `{"user_id":"u1","total_cents":100}`)
	if r1.Code != 201 {
		t.Fatalf("setup failed: %d", r1.Code)
	}
	r2 := doOrder(t, "0123456789abcdef", `{"user_id":"u1","total_cents":999}`)
	if r2.Code != 409 {
		t.Fatalf("expected 409 IDEMPOTENCY_CONFLICT, got %d: %s", r2.Code, r2.Body.String())
	}
	if !strings.Contains(r2.Body.String(), "IDEMPOTENCY_CONFLICT") {
		t.Fatalf("missing code: %s", r2.Body.String())
	}
}

func TestWriteNotOwned(t *testing.T) {
	resetStore()
	ownershipOverride = "other" // simulate Azure shadow during read-only canary
	rec := doOrder(t, "0123456789abcdef", `{"user_id":"u1","total_cents":100}`)
	if rec.Code != 403 {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "WRITE_NOT_OWNED") {
		t.Fatalf("missing code: %s", rec.Body.String())
	}
}

func TestReadyzPayload(t *testing.T) {
	resetStore()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	readyz(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["schema_version"] != float64(1) {
		t.Fatalf("schema_version: %v", payload["schema_version"])
	}
	if payload["write_ownership"] != "aws" {
		t.Fatalf("write_ownership: %v", payload["write_ownership"])
	}
}

func TestLazyWarmCache(t *testing.T) {
	resetStore()
	var id string
	for _, p := range store.ListProducts() {
		id = p.ID
	}
	miss := httptest.NewRecorder()
	getProduct(miss, httptest.NewRequest(http.MethodGet, "/v1/products/"+id, nil))
	if miss.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("expected MISS, got %q", miss.Header().Get("X-Cache"))
	}
	hit := httptest.NewRecorder()
	getProduct(hit, httptest.NewRequest(http.MethodGet, "/v1/products/"+id, nil))
	if hit.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("expected HIT, got %q", hit.Header().Get("X-Cache"))
	}
}
