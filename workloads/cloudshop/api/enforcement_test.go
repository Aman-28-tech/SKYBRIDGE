// Ownership enforcement proof: deterministic end-to-end of the §1 A–E
// contract. Distinguishes ownership (403 WRITE_NOT_OWNED), quiesce (503
// WRITES_PAUSED), and validation (400) rejections explicitly.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postOrderCode(t *testing.T, key, body string) (int, string) {
	t.Helper()
	rec := doOrder(t, key, body)
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return rec.Code, env.Error.Code
}

// TestOwnershipEnforcementProof walks A->B->C->D->E in one flow.
func TestOwnershipEnforcementProof(t *testing.T) {
	resetStore()
	setQuiesced(false)
	if _, ok, _ := setOwnership("aws"); !ok {
		t.Fatal("setup")
	}
	defer resetOwnership(t)

	// A: AWS authoritative + unquiesced -> AWS write succeeds.
	t.Setenv("DEPLOYMENT", "aws")
	ownershipOverride = ""
	if code, _ := postOrderCode(t, "enfproofkey00001", `{"user_id":"u1","total_cents":100}`); code != 201 {
		t.Fatalf("A: aws write must succeed, got %d", code)
	}

	// Transfer (control-plane drives this in production; direct here).
	if rec := postOwnership(t, "azure"); rec.Code != 200 {
		t.Fatalf("transfer: %d", rec.Code)
	}

	// C: AWS explicitly UNquiesced, yet AWS write still fails on ownership
	// (403), never on quiesce (503) and never succeeds.
	setQuiesced(false)
	if q, _ := getQuiesce(t); q {
		t.Fatal("C requires AWS unquiesced")
	}
	code, errCode := postOrderCode(t, "enfproofkey00002", `{"user_id":"u1","total_cents":100}`)
	if code != 403 || errCode != "WRITE_NOT_OWNED" {
		t.Fatalf("C: want 403 WRITE_NOT_OWNED, got %d %s", code, errCode)
	}

	// D: Azure write succeeds after the AWS rejection.
	t.Setenv("DEPLOYMENT", "azure")
	if code, _ := postOrderCode(t, "enfproofkey00003", `{"user_id":"u1","total_cents":100}`); code != 201 {
		t.Fatalf("D: azure write must succeed, got %d", code)
	}

	// E: both providers never accept simultaneously, in either ownership.
	for _, owner := range []string{"aws", "azure"} {
		if rec := postOwnership(t, owner); rec.Code != 200 {
			t.Fatalf("set %s: %d", owner, rec.Code)
		}
		accepts := 0
		for _, deploy := range []string{"aws", "azure"} {
			t.Setenv("DEPLOYMENT", deploy)
			setQuiesced(false)
			if code, _ := postOrderCode(t, "enfproofkey-"+owner+"-"+deploy, `{"user_id":"u1","total_cents":100}`); code == 201 {
				accepts++
			}
		}
		if accepts != 1 {
			t.Fatalf("E: owner=%s accepted by %d deployments, want exactly 1", owner, accepts)
		}
	}

	// Rejection causes stay distinct: quiesce (503) vs ownership (403) vs
	// validation (400) on the same deployment.
	t.Setenv("DEPLOYMENT", "aws")
	if _, ok, _ := setOwnership("aws"); !ok {
		t.Fatal("reset")
	}
	setQuiesced(true)
	if code, errCode := postOrderCode(t, "enfproofkey00004", `{"user_id":"u1","total_cents":100}`); code != 503 || errCode != "WRITES_PAUSED" {
		t.Fatalf("quiesce: got %d %s", code, errCode)
	}
	setQuiesced(false)
	if rec := postOwnership(t, "azure"); rec.Code != 200 {
		t.Fatal("transfer")
	}
	if code, errCode := postOrderCode(t, "enfproofkey00005", `{"user_id":"u1","total_cents":100}`); code != 403 || errCode != "WRITE_NOT_OWNED" {
		t.Fatalf("ownership: got %d %s", code, errCode)
	}
	// Validation rejection needs owner + unquiesced context to reach the
	// body parser.
	if _, ok, _ := setOwnership("aws"); !ok {
		t.Fatal("reset")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(`not-json`))
	req.Header.Set("Idempotency-Key", "enfproofkey00006")
	createOrder(rec, req)
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if rec.Code != 400 || env.Error.Code != "VALIDATION_FAILED" {
		t.Fatalf("validation: got %d %s", rec.Code, env.Error.Code)
	}
}
