// Control-plane quiesce orchestration tests (run: go test ./...).
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeQuiesceClient struct {
	quiesced  bool
	ownership string
	err       error
	calls     int
	// ownershipCalls counts SetOwnership invocations; failOwnership makes
	// the next SetOwnership call fail (partial-transfer simulation).
	ownershipCalls int
	failOwnership  error
	// failUnquiesce fails the next SetQuiesced(false) call only.
	failUnquiesce error
	// failQuiesce fails the next SetQuiesced(true) call only.
	failQuiesce error
	// nonSticky simulates a lost admin write: SetOwnership reports
	// success without changing observed state.
	nonSticky bool
}

func (f *fakeQuiesceClient) SetOwnership(ctx context.Context, owner string) (string, error) {
	f.ownershipCalls++
	if f.failOwnership != nil {
		err := f.failOwnership
		f.failOwnership = nil
		return "", err
	}
	if owner != "aws" && owner != "azure" {
		return "", fmt.Errorf("invalid owner %q", owner)
	}
	if !f.nonSticky {
		f.ownership = owner
	}
	return owner, nil
}

func (f *fakeQuiesceClient) SetQuiesced(ctx context.Context, q bool) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	if q && f.failQuiesce != nil {
		err := f.failQuiesce
		f.failQuiesce = nil
		return false, err
	}
	if !q && f.failUnquiesce != nil {
		err := f.failUnquiesce
		f.failUnquiesce = nil
		return false, err
	}
	f.quiesced = q
	return f.quiesced, nil
}

func (f *fakeQuiesceClient) State(ctx context.Context) (bool, string, error) {
	if f.err != nil {
		return false, "", f.err
	}
	return f.quiesced, f.ownership, nil
}

func useQuiesceFake(f *fakeQuiesceClient) func() {
	old := quiesceClient
	quiesceClient = f
	return func() { quiesceClient = old }
}

// useQuiesceFakes swaps both source and target seams.
func useQuiesceFakes(src, tgt *fakeQuiesceClient) func() {
	oldSrc, oldTgt := quiesceClient, targetQuiesceClient
	quiesceClient, targetQuiesceClient = src, tgt
	return func() { quiesceClient, targetQuiesceClient = oldSrc, oldTgt }
}

func quiesceReq(t *testing.T, wid, migID, key, body string) *httptest.ResponseRecorder {
	return quiesceReqAs(t, wid, migID, key, body, "test-admin")
}

func quiesceReqAs(t *testing.T, wid, migID, key, body, actor string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+migID+"/quiesce", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	withBearer(req, actor, "")
	rec := httptest.NewRecorder()
	postQuiesce(rec, req, wid, migID)
	return rec
}

func quiesceChain(t *testing.T, key string) (wid, migID string) {
	t.Helper()
	resetStore()
	wid = registerPlanWID(t, key+"reg000001")
	migID = createMig(t, wid)
	return wid, migID
}

// Q: repeated identical transitions replay; unquiesce resumes.
func TestQuiesceRepeated(t *testing.T) {
	wid, migID := quiesceChain(t, "quiesceq000000001")
	f := &fakeQuiesceClient{ownership: "aws"}
	defer useQuiesceFake(f)()
	for _, k := range []string{"quiesceqkey000001", "quiesceqkey000002", "quiesceqkey000003"} {
		rec := quiesceReq(t, wid, migID, k, `{"quiesced":true}`)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"quiesced":true`) {
			t.Fatalf("quiesce %s: %d %s", k, rec.Code, rec.Body.String())
		}
	}
	rec := quiesceReq(t, wid, migID, "quiesceqkey000004", `{"quiesced":false}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"quiesced":false`) {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body.String())
	}
	if f.calls != 4 {
		t.Fatalf("calls=%d", f.calls)
	}
}

// R: concurrent same-key transitions collapse to one call.
func TestQuiesceConcurrent(t *testing.T) {
	wid, migID := quiesceChain(t, "quiescer000000001")
	f := &fakeQuiesceClient{ownership: "aws"}
	defer useQuiesceFake(f)()
	const n = 8
	out := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = quiesceReq(t, wid, migID, "quiescerkey000001", `{"quiesced":true}`).Body.String()
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if out[i] != out[0] {
			t.Fatal("concurrent quiesce diverged")
		}
	}
	if f.calls != 1 {
		t.Fatalf("concurrent quiesce executed %d times", f.calls)
	}
}

// Unavailable CloudShop surfaces 503 without recording success audit.
func TestQuiesceUnavailable(t *testing.T) {
	wid, migID := quiesceChain(t, "quiesceu000000001")
	f := &fakeQuiesceClient{err: errFake("connection refused")}
	defer useQuiesceFake(f)()
	rec := quiesceReq(t, wid, migID, "quiesceukey0000001", `{"quiesced":true}`)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "QUIESCE_UNAVAILABLE") {
		t.Fatalf("unavailable: %d %s", rec.Code, rec.Body.String())
	}
}

// Idempotency conflict on changed body.
func TestQuiesceIdempotency(t *testing.T) {
	wid, migID := quiesceChain(t, "quiescei000000001")
	f := &fakeQuiesceClient{ownership: "aws"}
	defer useQuiesceFake(f)()
	r1 := quiesceReq(t, wid, migID, "quiesceikey000001", `{"quiesced":true}`)
	r2 := quiesceReq(t, wid, migID, "quiesceikey000001", `{"quiesced":true}`)
	if r1.Code != 200 || r2.Code != 200 || r1.Body.String() != r2.Body.String() {
		t.Fatal("replay broken")
	}
	r3 := quiesceReq(t, wid, migID, "quiesceikey000001", `{"quiesced":false}`)
	if r3.Code != 409 {
		t.Fatalf("conflict: %d", r3.Code)
	}
	if f.calls != 1 {
		t.Fatalf("replay re-executed: %d", f.calls)
	}
}
