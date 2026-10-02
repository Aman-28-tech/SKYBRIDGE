// Cutover observability: stable in-process counters, a read-only status
// endpoint, and structured stage logs. Observability is never an
// authorization source: the status endpoint reads facts, gates re-evaluate
// them. Metric names are stable with bounded labels (fixed stage set, no
// user input in labels). No credentials exist in this scope; positions and
// IDs are safe to log.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
)

// cutoverStats counts cutover lifecycle events process-wide.
type cutoverStatsT struct {
	mu               sync.Mutex
	attempts         uint64
	completed        uint64
	blocked          uint64
	rollbackRejected uint64
	failByStage      map[string]uint64
}

var cutoverStats = &cutoverStatsT{failByStage: map[string]uint64{}}

func (c *cutoverStatsT) attempt() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts++
}

func (c *cutoverStatsT) finish(status, stage string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if status == CutoverComplete {
		c.completed++
	} else {
		c.blocked++
		if stage != "" {
			c.failByStage[stage]++
		}
	}
}

func (c *cutoverStatsT) rollbackDenied() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollbackRejected++
}

func (c *cutoverStatsT) snapshot() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	byStage := map[string]uint64{}
	for k, v := range c.failByStage {
		byStage[k] = v
	}
	return map[string]any{
		"cutover_attempts":          c.attempts,
		"cutover_completed":         c.completed,
		"cutover_blocked":           c.blocked,
		"rollback_rejected":         c.rollbackRejected,
		"cutover_failures_by_stage": byStage,
	}
}

// cutoverLog emits one structured JSON line per event. Fields are bounded:
// stage names, owners, positions, lag. Never credentials.
func cutoverLog(event, migrationID, workloadID, requestID string, fields map[string]any) {
	rec := map[string]any{
		"ts":           time.Now().UTC().Format(time.RFC3339),
		"component":    "cutover",
		"event":        event,
		"migration_id": migrationID,
		"workload_id":  workloadID,
		"request_id":   requestID,
	}
	for k, v := range fields {
		rec[k] = v
	}
	b, _ := json.Marshal(rec)
	log.Print(string(b))
}

// getCutoverStatus implements GET /v1/migrations/{id}/cutover (read-only).
// Reports the ownership fact (absent = aws), routing, measured positions,
// latest canary verdict, both CloudShop quiesce states (unknown on error),
// and process counters.
func getCutoverStatus(w http.ResponseWriter, r *http.Request, workloadID, migrationID string) {
	rid := reqID(r)
	if _, ok := store.GetMigration(migrationID); !ok {
		writeErr(w, rid, "NOT_FOUND", "migration not found", 404)
		return
	}
	owner, routing, srcLSN, appliedLSN, lag := "aws", "aws", "", "", int64(0)
	if rec, ok := store.GetOwnership(migrationID); ok {
		owner, routing = rec.CurrentOwner, rec.Routing
		srcLSN, appliedLSN, lag = rec.SourceLSN, rec.AppliedLSN, rec.LagSeconds
	}
	latestStage, latestVerdict := -1, ""
	if rows := store.GetCanaryRecords(migrationID); len(rows) > 0 {
		latestStage, latestVerdict = rows[len(rows)-1].Stage, rows[len(rows)-1].Verdict
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	srcQ, _, srcErr := quiesceClient.State(ctx)
	tgtQ, _, tgtErr := targetQuiesceClient.State(ctx)
	srcState, tgtState := "unknown", "unknown"
	if srcErr == nil {
		srcState = map[bool]string{true: "quiesced", false: "accepting"}[srcQ]
	}
	if tgtErr == nil {
		tgtState = map[bool]string{true: "quiesced", false: "accepting"}[tgtQ]
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"migration_id": migrationID, "workload_id": workloadID,
		"current_owner": owner, "routing": routing,
		"source_lsn": srcLSN, "applied_lsn": appliedLSN, "cdc_lag_seconds": lag,
		"canary_latest_stage": latestStage, "canary_latest_verdict": latestVerdict,
		"source_write_state": srcState, "target_write_state": tgtState,
		"policy_bundle_version": policyBundleVersion(),
		"counters":              cutoverStats.snapshot(),
		"request_id":            rid,
	})
}

// sortedKeys renders bounded label sets deterministically (tests/docs).
func sortedKeys(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
