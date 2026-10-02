// Ownership transfer tests: split-brain invariant + post-cutover write
// matrix (run: go test ./...).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postOwnership(t *testing.T, owner string) *httptest.ResponseRecorder {
	return postOwnershipAs(t, owner, "test-shop-admin-0001")
}

func postOwnershipAs(t *testing.T, owner, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/ownership",
		strings.NewReader(`{"write_ownership":"`+owner+`"}`))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	ownershipAdmin(rec, req)
	return rec
}

func resetOwnership(t *testing.T) {
	t.Helper()
	if _, err := setQuiesced(false); err != nil {
		t.Fatal("reset quiesce")
	}
	if _, ok, err := setOwnership(ownerOf(t)); err != nil || !ok {
		t.Fatal("reset ownership")
	}
	ownershipOverride = ""
}

func ownerOf(t *testing.T) string {
	t.Helper()
	if v := getEnv("WRITE_OWNERSHIP", "aws"); v == "aws" || v == "azure" {
		return v
	}
	return "aws"
}

// Q: split-brain invariant. Exactly one deployment accepts authoritative
// writes for a given ownership value.
func TestSplitBrainInvariant(t *testing.T) {
	resetStore()
	defer resetOwnership(t)
	for _, owner := range []string{"aws", "azure"} {
		if rec := postOwnership(t, owner); rec.Code != 200 {
			t.Fatalf("set %s: %d", owner, rec.Code)
		}
		for _, deploy := range []string{"aws", "azure"} {
			t.Setenv("DEPLOYMENT", deploy)
			ownershipOverride = ""
			rec := doOrder(t, "splitbraink000001", `{"user_id":"u1","total_cents":100}`)
			want := 403
			if deploy == owner {
				setQuiesced(false)
				want = 201
			}
			if rec.Code != want {
				t.Fatalf("owner=%s deploy=%s: got %d want %d: %s", owner, deploy, rec.Code, want, rec.Body.String())
			}
		}
	}
}

// Invalid ownership values are rejected; state unchanged.
func TestOwnershipValidation(t *testing.T) {
	resetStore()
	defer resetOwnership(t)
	before := currentOwnership()
	if rec := postOwnership(t, "gcp"); rec.Code != 400 {
		t.Fatalf("invalid owner: %d", rec.Code)
	}
	if currentOwnership() != before {
		t.Fatal("rejected transfer must not mutate ownership")
	}
}

// R: after transfer to azure, AWS-side authoritative writes are rejected.
func TestPostCutoverAWSWriteRejection(t *testing.T) {
	resetStore()
	defer resetOwnership(t)
	setQuiesced(false)
	if rec := postOwnership(t, "azure"); rec.Code != 200 {
		t.Fatalf("transfer: %d", rec.Code)
	}
	t.Setenv("DEPLOYMENT", "aws")
	ownershipOverride = ""
	rec := doOrder(t, "postcutaws0000001", `{"user_id":"u1","total_cents":100}`)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "WRITE_NOT_OWNED") {
		t.Fatalf("aws write after transfer: %d %s", rec.Code, rec.Body.String())
	}
}

// S: after transfer to azure, Azure-side writes succeed.
func TestPostCutoverAzureWriteSuccess(t *testing.T) {
	resetStore()
	defer resetOwnership(t)
	setQuiesced(false)
	if rec := postOwnership(t, "azure"); rec.Code != 200 {
		t.Fatalf("transfer: %d", rec.Code)
	}
	t.Setenv("DEPLOYMENT", "azure")
	ownershipOverride = ""
	rec := doOrder(t, "postcutaz00000001", `{"user_id":"u1","total_cents":100}`)
	if rec.Code != 201 {
		t.Fatalf("azure write after transfer: %d %s", rec.Code, rec.Body.String())
	}
	var o Order
	if err := json.Unmarshal(rec.Body.Bytes(), &o); err != nil || o.ID == "" {
		t.Fatalf("order: %v %s", err, rec.Body.String())
	}
	if _, ok := store.GetOrder(o.ID); !ok {
		t.Fatal("azure write must persist")
	}
}
