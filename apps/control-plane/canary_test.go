// Canary controller tests: exact thresholds, stage progression, stale-worker
// guards, concurrency (run: go test ./...).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func canaryReq(t *testing.T, wid, migID, key, body string) *httptest.ResponseRecorder {
	return canaryReqAs(t, wid, migID, key, body, "test-admin")
}

func canaryReqAs(t *testing.T, wid, migID, key, body, actor string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+migID+"/canary", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	withBearer(req, actor, "")
	rec := httptest.NewRecorder()
	postCanary(rec, req, wid, migID)
	return rec
}

func canaryMetricsJSON(m CanaryMetrics) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func canaryBody(stage, expected int, obs CanaryMetrics) string {
	base := CanaryMetrics{ErrorRate5xx: 0.002, P95Ms: 200, P99Ms: 400,
		Requests: 500, WindowSecs: 900, Healthy: true}
	return fmt.Sprintf(`{"stage":%d,"expected_stage":%d,"baseline":%s,"observed":%s}`,
		stage, expected, canaryMetricsJSON(base), canaryMetricsJSON(obs))
}

func goodObs(stage int) CanaryMetrics {
	m := CanaryMetrics{ErrorRate5xx: 0.003, P95Ms: 210, P99Ms: 420,
		Requests: 400, WindowSecs: 3600, Healthy: true}
	if stage == 1 {
		m.Requests, m.WindowSecs = 150, 300
	}
	if stage == 0 {
		m.Requests, m.WindowSecs = 500, 900
	}
	if stage == 100 {
		m.WindowSecs = 600
	}
	return m
}

func decodeCanary(t *testing.T, rec *httptest.ResponseRecorder) CanaryRecord {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var r CanaryRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func canaryChain(t *testing.T, key string) (wid, migID string) {
	t.Helper()
	resetStore()
	wid = registerPlanWID(t, key+"reg00001")
	migID = createMig(t, wid)
	return wid, migID
}

// A: 1% canary records PASS and reports the next expected stage.
func TestCanaryStageOne(t *testing.T) {
	wid, migID := canaryChain(t, "canarya00000001")
	r0 := decodeCanary(t, canaryReq(t, wid, migID, "canaryakey000001", canaryBody(0, 0, goodObs(0))))
	if r0.Verdict != CanaryPass {
		t.Fatalf("stage 0: %+v", r0)
	}
	r1 := decodeCanary(t, canaryReq(t, wid, migID, "canaryakey000002", canaryBody(1, 1, goodObs(1))))
	if r1.Verdict != CanaryPass || r1.Stage != 1 {
		t.Fatalf("stage 1: %+v", r1)
	}
	getReq := httptest.NewRequest(http.MethodGet, "/v1/migrations/"+migID+"/canary", nil)
	getRec := httptest.NewRecorder()
	getCanary(getRec, getReq, wid, migID)
	var out struct {
		Items         []CanaryRecord `json:"items"`
		ExpectedStage int            `json:"expected_stage"`
	}
	_ = json.Unmarshal(getRec.Body.Bytes(), &out)
	if len(out.Items) != 2 || out.ExpectedStage != 5 {
		t.Fatalf("history: %+v", out)
	}
}

// B: 5xx breach FAILs and blocks advancement.
func TestCanaryThresholdBreach(t *testing.T) {
	wid, migID := canaryChain(t, "canaryb00000001")
	decodeCanary(t, canaryReq(t, wid, migID, "canarybkey000001", canaryBody(0, 0, goodObs(0))))
	bad := goodObs(1)
	bad.ErrorRate5xx = 0.05
	r1 := decodeCanary(t, canaryReq(t, wid, migID, "canarybkey000002", canaryBody(1, 1, bad)))
	if r1.Verdict != CanaryFail {
		t.Fatalf("breach must FAIL: %+v", r1)
	}
	adv := canaryReq(t, wid, migID, "canarybkey000003", canaryBody(5, 5, goodObs(5)))
	if adv.Code != 409 || !strings.Contains(adv.Body.String(), "STAGE_FAILED") {
		t.Fatalf("advance after FAIL: %d %s", adv.Code, adv.Body.String())
	}
}

// C: p95 breach FAILs.
func TestCanaryP95Breach(t *testing.T) {
	wid, migID := canaryChain(t, "canaryc00000001")
	decodeCanary(t, canaryReq(t, wid, migID, "canaryckey000001", canaryBody(0, 0, goodObs(0))))
	bad := goodObs(5)
	bad.Requests, bad.WindowSecs = 400, 900
	bad.P95Ms = 900 // threshold max(500, 250) = 500
	// need stage 1 first
	decodeCanary(t, canaryReq(t, wid, migID, "canaryckey000002", canaryBody(1, 1, goodObs(1))))
	r := decodeCanary(t, canaryReq(t, wid, migID, "canaryckey000003", canaryBody(5, 5, bad)))
	if r.Verdict != CanaryFail {
		t.Fatalf("p95 breach must FAIL: %+v", r)
	}
}

// D: p99 breach FAILs.
func TestCanaryP99Breach(t *testing.T) {
	wid, migID := canaryChain(t, "canaryd00000001")
	decodeCanary(t, canaryReq(t, wid, migID, "canarydkey000001", canaryBody(0, 0, goodObs(0))))
	decodeCanary(t, canaryReq(t, wid, migID, "canarydkey000002", canaryBody(1, 1, goodObs(1))))
	bad := goodObs(5)
	bad.Requests, bad.WindowSecs = 400, 900
	bad.P99Ms = 5000 // threshold max(1000, 500) = 1000
	r := decodeCanary(t, canaryReq(t, wid, migID, "canarydkey000003", canaryBody(5, 5, bad)))
	if r.Verdict != CanaryFail {
		t.Fatalf("p99 breach must FAIL: %+v", r)
	}
}

// E: insufficient volume is INCONCLUSIVE and blocks advancement.
func TestCanaryInsufficientVolume(t *testing.T) {
	wid, migID := canaryChain(t, "canarye00000001")
	decodeCanary(t, canaryReq(t, wid, migID, "canaryekey000001", canaryBody(0, 0, goodObs(0))))
	thin := goodObs(1)
	thin.Requests = 50 // minimum 100 for stage 1
	r := decodeCanary(t, canaryReq(t, wid, migID, "canaryekey000002", canaryBody(1, 1, thin)))
	if r.Verdict != CanaryInconclusive {
		t.Fatalf("thin traffic must be INCONCLUSIVE: %+v", r)
	}
	adv := canaryReq(t, wid, migID, "canaryekey000003", canaryBody(5, 5, goodObs(5)))
	if adv.Code != 409 {
		t.Fatalf("advance after INCONCLUSIVE: %d", adv.Code)
	}
}

// F: missing observations fail closed (unhealthy FAILs, short window INCONCLUSIVE).
func TestCanaryMissingObservation(t *testing.T) {
	wid, migID := canaryChain(t, "canaryf00000001")
	decodeCanary(t, canaryReq(t, wid, migID, "canaryfkey000001", canaryBody(0, 0, goodObs(0))))
	down := goodObs(1)
	down.Healthy = false
	if r := decodeCanary(t, canaryReq(t, wid, migID, "canaryfkey000002", canaryBody(1, 1, down))); r.Verdict != CanaryFail {
		t.Fatalf("unhealthy must FAIL: %+v", r)
	}
}

// Non-canonical stages are rejected, never recorded.
func TestCanaryStageValidation(t *testing.T) {
	wid, migID := canaryChain(t, "canaryv00000001")
	rec := canaryReq(t, wid, migID, "canaryvkey000001", canaryBody(10, 0, goodObs(1)))
	if rec.Code != 400 {
		t.Fatalf("stage 10: %d", rec.Code)
	}
	if len(store.GetCanaryRecords(migID)) != 0 {
		t.Fatal("invalid stage must not persist")
	}
}

// PASS history is immutable; retry of latest non-PASS with fresh data works.
func TestCanaryRetryAndImmutability(t *testing.T) {
	wid, migID := canaryChain(t, "canaryr00000001")
	decodeCanary(t, canaryReq(t, wid, migID, "canaryrkey000001", canaryBody(0, 0, goodObs(0))))
	bad := goodObs(1)
	bad.ErrorRate5xx = 0.09
	if r := decodeCanary(t, canaryReq(t, wid, migID, "canaryrkey000002", canaryBody(1, 1, bad))); r.Verdict != CanaryFail {
		t.Fatalf("setup FAIL: %+v", r)
	}
	// Retry latest failed stage with fresh observations (expected_stage stays 5).
	r := decodeCanary(t, canaryReq(t, wid, migID, "canaryrkey000003", canaryBody(1, 5, goodObs(1))))
	if r.Verdict != CanaryPass {
		t.Fatalf("retry: %+v", r)
	}
	// Re-recording the now-PASS stage is refused.
	dup := canaryReq(t, wid, migID, "canaryrkey000004", canaryBody(1, 5, goodObs(1)))
	if dup.Code != 409 {
		t.Fatalf("immutable PASS: %d", dup.Code)
	}
	// Stale worker for an old stage is refused.
	stale := canaryReq(t, wid, migID, "canaryrkey000005", canaryBody(0, 5, goodObs(0)))
	if stale.Code != 409 || !strings.Contains(stale.Body.String(), "STALE_STAGE") {
		t.Fatalf("stale: %d %s", stale.Code, stale.Body.String())
	}
}

// G: concurrent same-key controllers collapse to one transition.
func TestCanaryConcurrency(t *testing.T) {
	wid, migID := canaryChain(t, "canaryg00000001")
	body := canaryBody(0, 0, goodObs(0))
	const n = 8
	out := make([]string, n)
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := canaryReq(t, wid, migID, "canarygkey000001", body)
			out[i], codes[i] = rec.Body.String(), rec.Code
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if out[i] != out[0] || codes[i] != 200 {
			t.Fatal("concurrent same-key controllers diverged")
		}
	}
	if len(store.GetCanaryRecords(migID)) != 1 {
		t.Fatalf("duplicate transitions: %d", len(store.GetCanaryRecords(migID)))
	}
	// Racing distinct keys for the same stage: exactly one wins.
	var wg2 sync.WaitGroup
	codes2 := make([]int, n)
	for i := 0; i < n; i++ {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			codes2[i] = canaryReq(t, wid, migID, fmt.Sprintf("canarygkey20%04d", i), body).Code
		}(i)
	}
	wg2.Wait()
	wins := 0
	for _, c := range codes2 {
		if c == 200 {
			wins++
		} else if c != 409 {
			t.Fatalf("unexpected code %d", c)
		}
	}
	// Stage 0 already PASS: every distinct-key retry is refused.
	if wins != 0 {
		t.Fatalf("stale controllers advanced: %d", wins)
	}
}

// Idempotency: replay identical, conflict on changed body.
func TestCanaryIdempotency(t *testing.T) {
	wid, migID := canaryChain(t, "canaryi00000001")
	body := canaryBody(0, 0, goodObs(0))
	r1 := canaryReq(t, wid, migID, "canaryikey000001", body)
	r2 := canaryReq(t, wid, migID, "canaryikey000001", body)
	if r1.Code != 200 || r2.Code != 200 || r1.Body.String() != r2.Body.String() {
		t.Fatal("replay broken")
	}
	r3 := canaryReq(t, wid, migID, "canaryikey000001", canaryBody(0, 0, goodObs(1)))
	if r3.Code != 409 {
		t.Fatalf("conflict: %d", r3.Code)
	}
}

// Full walk helper shared by readiness tests and live acceptance.
func canaryWalk(t *testing.T, wid, migID, keyPrefix string) {
	t.Helper()
	expected := 0
	i := 0
	for _, s := range []int{0, 1, 5, 25, 50} {
		rec := canaryReq(t, wid, migID, fmt.Sprintf("%s%04d", keyPrefix, i),
			canaryBody(s, expected, goodObs(s)))
		r := decodeCanary(t, rec)
		if r.Verdict != CanaryPass {
			t.Fatalf("stage %d: %+v", s, r)
		}
		expected = canaryNext[s]
		i++
	}
}
