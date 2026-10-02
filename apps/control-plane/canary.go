// Canary controller: deterministic staged evaluation over the canonical
// stages 0/1/5/25/50/100 (authority: docs/CUTOVER_ROUTING.md; do not invent
// other thresholds). Stages 0-50 are read-only evaluations; the 100 stage
// evaluates thresholds only and never transfers ownership here.
//
// Thresholds (exact, from CUTOVER_ROUTING.md Gates):
//   target_5xx <= max(1.0%, baseline_5xx + 0.5pp)
//   target_p95 <= max(500ms, baseline_p95 * 1.25)
//   target_p99 <= max(1s, baseline_p99 * 1.25)
// Volume: >=300 requests per stage (>=100 for stage 1); below minimum (or
// missing observations, short windows, unhealthy target) the verdict is
// INCONCLUSIVE — never PASS. Any breach is FAIL and blocks advancement.
//
// Persistence: records are history (one row per evaluation). Advancement
// requires the latest verdict to be PASS and the requested stage to be the
// expected next. Re-recording a PASS stage is refused (409); retrying the
// latest non-PASS stage with fresh observations is allowed. Per-migration
// locking serializes concurrent controllers; stale expected_stage is 409.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

// Canonical stages (CUTOVER_ROUTING.md). Stage 0 is the baseline window.
var canaryStages = []int{0, 1, 5, 25, 50, 100}

// canaryNext maps a recorded stage to its expected successor.
var canaryNext = map[int]int{0: 1, 1: 5, 5: 25, 25: 50, 50: 100}

// canaryMinRequests is the minimum volume per stage (100 for stage 1).
func canaryMinRequests(stage int) int64 {
	if stage == 1 {
		return 100
	}
	return 300
}

// canaryMinWindowSecs is the required observation window per stage.
// Stages 1-4 from CUTOVER_ROUTING.md (5/15/30/60 min); stage 0 records the
// 15-min baseline window; stage 100 uses the 10-min validation-suite window
// (WORKFLOW_STATE_MACHINE.md) as final validation.
func canaryMinWindowSecs(stage int) int64 {
	switch stage {
	case 0:
		return 900
	case 1:
		return 300
	case 5:
		return 900
	case 25:
		return 1800
	case 50:
		return 3600
	case 100:
		return 600
	default:
		return 1<<62 - 1 // unknown stage can never satisfy
	}
}

// Canary verdicts.
const (
	CanaryPass         = "PASS"
	CanaryFail         = "FAIL"
	CanaryInconclusive = "INCONCLUSIVE"
)

// CanaryMetrics is one observation set. Rates are fractions (0.008 = 0.8%),
// latencies milliseconds. Healthy=false (or absent data) fails closed.
type CanaryMetrics struct {
	ErrorRate5xx float64 `json:"error_rate_5xx"`
	P95Ms        float64 `json:"p95_ms"`
	P99Ms        float64 `json:"p99_ms"`
	Requests     int64   `json:"requests"`
	WindowSecs   int64   `json:"window_secs"`
	Healthy      bool    `json:"healthy"`
}

// CanaryRecord is one persisted stage evaluation.
type CanaryRecord struct {
	ID          string        `json:"id"`
	MigrationID string        `json:"migration_id"`
	WorkloadID  string        `json:"workload_id"`
	Stage       int           `json:"stage"`
	Verdict     string        `json:"verdict"`
	Baseline    CanaryMetrics `json:"baseline"`
	Observed    CanaryMetrics `json:"observed"`
	Reasons     []string      `json:"reasons"`
	RequestID   string        `json:"request_id"`
	CreatedAt   string        `json:"created_at"`
}

// evaluateCanary applies the exact documented thresholds. Stage 0 records the
// baseline (PASS by definition once volume/window/health hold).
func evaluateCanary(stage int, baseline, observed CanaryMetrics) (string, []string) {
	reasons := []string{}
	inconclusive := func(r string) (string, []string) {
		return CanaryInconclusive, append(reasons, r)
	}
	fail := func(r string) (string, []string) {
		return CanaryFail, append(reasons, r)
	}
	if stage == 0 {
		if !observed.Healthy {
			return inconclusive("baseline unhealthy: no baseline")
		}
		if observed.Requests < canaryMinRequests(1) {
			return inconclusive(fmt.Sprintf("baseline volume %d < 100", observed.Requests))
		}
		if observed.WindowSecs < canaryMinWindowSecs(0) {
			return inconclusive("baseline window short")
		}
		return CanaryPass, []string{"baseline recorded"}
	}
	if !observed.Healthy {
		return fail("target unhealthy")
	}
	if observed.Requests < canaryMinRequests(stage) {
		return inconclusive(fmt.Sprintf("volume %d < minimum %d", observed.Requests, canaryMinRequests(stage)))
	}
	if observed.WindowSecs < canaryMinWindowSecs(stage) {
		return inconclusive(fmt.Sprintf("window %ds < required %ds", observed.WindowSecs, canaryMinWindowSecs(stage)))
	}
	errT := 0.01
	if baseline.ErrorRate5xx+0.005 > errT {
		errT = baseline.ErrorRate5xx + 0.005
	}
	if observed.ErrorRate5xx > errT {
		return fail(fmt.Sprintf("5xx %.4f > threshold %.4f", observed.ErrorRate5xx, errT))
	}
	p95T := 500.0
	if baseline.P95Ms*1.25 > p95T {
		p95T = baseline.P95Ms * 1.25
	}
	if observed.P95Ms > p95T {
		return fail(fmt.Sprintf("p95 %.1fms > threshold %.1fms", observed.P95Ms, p95T))
	}
	p99T := 1000.0
	if baseline.P99Ms*1.25 > p99T {
		p99T = baseline.P99Ms * 1.25
	}
	if observed.P99Ms > p99T {
		return fail(fmt.Sprintf("p99 %.1fms > threshold %.1fms", observed.P99Ms, p99T))
	}
	return CanaryPass, []string{"all thresholds satisfied"}
}

// latestCanary returns the most recent record for a migration.
func latestCanary(migrationID string) (CanaryRecord, bool) {
	rows := store.GetCanaryRecords(migrationID)
	if len(rows) == 0 {
		return CanaryRecord{}, false
	}
	return rows[len(rows)-1], true
}

// postCanary implements POST /v1/migrations/{id}/canary.
// Body: {"stage":int,"expected_stage":int,"baseline":{...},"observed":{...}}.
// expected_stage guards stale workers: it must equal the controller's
// expected next stage (0 when no records yet; GET reports it). Recording the
// expected next stage advances; re-recording the latest stage is allowed only
// while its verdict is not PASS (retry with fresh observations). PASS history
// is immutable, and a non-PASS latest blocks advancement.
func postCanary(w http.ResponseWriter, r *http.Request, workloadID, migrationID string) {
	rid := reqID(r)
	actor, ok := requireAuth(w, r)
	if !ok {
		return
	}
	// H-2: the migration must bind to the route workload and the caller must
	// be authorized for it. Cross-workload / cross-migration use is rejected.
	if _, ok := requireMigrationAccess(w, r, actor, migrationID, workloadID); !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 {
		writeErr(w, rid, "VALIDATION_FAILED", "Idempotency-Key required (min 16 chars)", 400)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "unreadable body", 400)
		return
	}
	hash, err := canonicalHash(raw)
	if err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "invalid JSON", 400)
		return
	}
	var in struct {
		Stage         int           `json:"stage"`
		ExpectedStage int           `json:"expected_stage"`
		Baseline      CanaryMetrics `json:"baseline"`
		Observed      CanaryMetrics `json:"observed"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "invalid JSON", 400)
		return
	}
	valid := false
	for _, s := range canaryStages {
		if in.Stage == s {
			valid = true
		}
	}
	if !valid {
		writeErr(w, rid, "VALIDATION_FAILED", "stage must be one of 0/1/5/25/50/100", 400)
		return
	}
	// Per-migration lock (not per request key): concurrent controllers
	// serialize; the loser observes the winner's record.
	mu := idemLock("canary:" + migrationID)
	mu.Lock()
	defer mu.Unlock()
	if resp, status, storedHash, found := store.CheckIdem("canary:"+key); found {
		if storedHash == "" || storedHash == hash {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(resp)
			return
		}
		idempotencyConflict(w, actor, "record_canary", key, rid, AuditEntry{WorkloadID: workloadID, RunID: migrationID})
		return
	}
	latest, hasLatest := latestCanary(migrationID)
	expected := 0
	if hasLatest {
		next, ok := canaryNext[latest.Stage]
		if !ok {
			writeErr(w, rid, "CANARY_COMPLETE", "canary already recorded through stage 100", 409)
			return
		}
		expected = next
		// Immutable history: a PASS stage is never re-recorded.
		if in.Stage != expected {
			if in.Stage == latest.Stage && latest.Verdict != CanaryPass {
				// Retry of the latest non-PASS stage with fresh observations.
			} else {
				writeErr(w, rid, "STALE_STAGE", fmt.Sprintf("expected stage %d, got %d", expected, in.Stage), 409)
				return
			}
		} else if latest.Verdict != CanaryPass {
			writeErr(w, rid, "STAGE_FAILED", fmt.Sprintf("stage %d verdict %s blocks advancement", latest.Stage, latest.Verdict), 409)
			return
		}
	}
	if in.ExpectedStage != expected {
		writeErr(w, rid, "STALE_STAGE", fmt.Sprintf("expected stage %d, worker sent %d", expected, in.ExpectedStage), 409)
		return
	}
	verdict, reasons := evaluateCanary(in.Stage, in.Baseline, in.Observed)
	rec := CanaryRecord{
		ID: uuid(), MigrationID: migrationID, WorkloadID: workloadID,
		Stage: in.Stage, Verdict: verdict, Baseline: in.Baseline, Observed: in.Observed,
		Reasons: reasons, RequestID: rid, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if rec.Reasons == nil {
		rec.Reasons = []string{}
	}
	if err := store.SaveCanaryRecord(rec); err != nil {
		writeErr(w, rid, "INTERNAL_ERROR", "canary persist failed", 500)
		return
	}
	result := "success"
	if verdict != CanaryPass {
		result = "denied"
	}
	_ = store.RecordAudit(AuditEntry{
		RunID: migrationID, WorkloadID: workloadID, ActorType: actor.Type, ActorID: actor.ID,
		Action: "record_canary_stage", RequestID: rid, IdempotencyKey: key,
		PolicyBundleVersion: policyBundleVersion(), Result: result,
		Metadata: fmt.Sprintf(`{"stage":%d,"verdict":%q}`, in.Stage, verdict),
	})
	body, _ := json.Marshal(rec)
	store.SaveIdem("canary:"+key, hash, body, 200)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(body)
}

// getCanary implements GET /v1/migrations/{id}/canary (ordered history).
func getCanary(w http.ResponseWriter, r *http.Request, workloadID, migrationID string) {
	rid := reqID(r)
	if _, ok := store.GetMigration(migrationID); !ok {
		writeErr(w, rid, "NOT_FOUND", "migration not found", 404)
		return
	}
	rows := store.GetCanaryRecords(migrationID)
	if rows == nil {
		rows = []CanaryRecord{}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Stage != rows[j].Stage {
			return rows[i].Stage < rows[j].Stage
		}
		return rows[i].CreatedAt < rows[j].CreatedAt
	})
	latest, _ := latestCanary(migrationID)
	next := -1
	if len(rows) == 0 {
		next = 0
	} else if n, ok := canaryNext[latest.Stage]; ok && latest.Verdict == CanaryPass {
		next = n
	} else if latest.Verdict != CanaryPass {
		next = latest.Stage // retry current
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"items": rows, "latest": latest, "expected_stage": next,
	})
}

// canaryPassForStages reports whether every listed stage has a PASS record.
func canaryPassForStages(migrationID string, stages []int) bool {
	rows := store.GetCanaryRecords(migrationID)
	pass := map[int]bool{}
	for _, r := range rows {
		if r.Verdict == CanaryPass {
			pass[r.Stage] = true
		}
	}
	for _, s := range stages {
		if !pass[s] {
			return false
		}
	}
	return true
}
