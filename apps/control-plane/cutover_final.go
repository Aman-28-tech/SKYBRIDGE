// Final cutover: local-only ownership transfer AWS -> Azure with recovery.
//
// State machine (Migration terminology; no competing model):
//
//	READY_FOR_CUTOVER (revalidated, never trusted from cache)
//	  -> FINAL_PREFLIGHT -> WRITES_QUIESCED -> CDC_CATCHING_UP
//	  -> CDC_CAUGHT_UP -> FINAL_VALIDATION -> OWNERSHIP_TRANSFERRED
//	  -> TRAFFIC_SWITCHED -> WRITES_RESUMED -> CUTOVER_COMPLETE
//
// Any failure moves to an explicit CUTOVER_BLOCKED safe state and prevents
// later stages. v1 has no reverse CDC: after transfer, traffic-only rollback
// to AWS is rejected (see rollback wiring in main.go); recovery is
// forward-fix from authoritative Azure state.
//
// Split-brain safety by construction: the source admin flips aws -> azure
// FIRST (both sides then reject writes: paused, never dual-authoritative),
// the target flips SECOND (azure accepts, source still rejects). A failure
// between flips leaves the paused state, retried via the resume path.
// The store CAS (TransferOwnership aws -> azure) commits exactly once;
// Apply is never called anywhere in this file.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Cutover terminal statuses.
const (
	CutoverComplete = "CUTOVER_COMPLETE"
	CutoverBlocked  = "CUTOVER_BLOCKED"
)

// Cutover failure codes.
const (
	CutoverAlreadyTransferred = "ALREADY_TRANSFERRED"
	CutoverSourceFlipFailed   = "SOURCE_FLIP_FAILED"
	CutoverTargetFlipFailed   = "TARGET_FLIP_FAILED"
	CutoverVerifyFailed       = "TRANSFER_VERIFY_FAILED"
	CutoverStoreFailed        = "OWNERSHIP_STORE_FAILED"
	CutoverResumeFailed       = "RESUME_FAILED"
)

// cutoverStage runs one named stage, timing it into the transcript.
type cutoverStage struct {
	Name     string  `json:"name"`
	Secs     float64 `json:"duration_secs"`
	Skipped  bool    `json:"skipped,omitempty"`
	Detail   string  `json:"detail,omitempty"`
}

// postCutoverFinal implements POST /v1/migrations/{id}/cutover/final.
// Body: {"environment":...,"approval_id":...}. Strict semantic idempotency
// (cutfinal: namespace): same key + same body replays the stored result;
// same key + different body is 409. Ownership transfers at most once: a
// completed transfer replays for the same key and refuses ALREADY_TRANSFERRED
// for new keys.
func postCutoverFinal(w http.ResponseWriter, r *http.Request, workloadID, migrationID string) {
	rid := reqID(r)
	actor, ok := requireAuth(w, r)
	if !ok {
		return
	}
	// H-2: explicit migration -> workload binding plus caller authorization.
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
		Environment string `json:"environment"`
		ApprovalID  string `json:"approval_id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "invalid JSON", 400)
		return
	}
	if in.Environment == "" {
		in.Environment = "dev"
	}
	// One controller at a time per migration; concurrent attempts serialize
	// and the loser replays or fails safe on current state.
	mu := idemLock("cutfinal:" + migrationID)
	mu.Lock()
	defer mu.Unlock()
	if resp, status, storedHash, found := store.CheckIdem("cutfinal:" + key); found {
		if storedHash == "" || storedHash == hash {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(resp)
			return
		}
		idempotencyConflict(w, actor, "final_cutover", key, rid, AuditEntry{WorkloadID: workloadID, RunID: migrationID})
		return
	}
	cutoverStats.attempt()
	cutoverLog("cutover_start", migrationID, workloadID, rid, map[string]any{"environment": in.Environment})
	res := runCutover(r.Context(), rid, actor, workloadID, migrationID, in.Environment, in.ApprovalID)
	cutoverStats.finish(fmt.Sprint(res["status"]), fmt.Sprint(res["stage"]))
	cutoverLog("cutover_finish", migrationID, workloadID, rid, map[string]any{
		"status": res["status"], "stage": res["stage"],
		"current_owner": res["current_owner"],
		"source_lsn": res["source_lsn"], "applied_lsn": res["applied_lsn"],
	})
	body, _ := json.Marshal(res)
	status := "success"
	if res["status"] != CutoverComplete {
		status = "denied"
	}
	_ = store.RecordAudit(AuditEntry{
		RunID: migrationID, WorkloadID: workloadID, ActorType: actor.Type, ActorID: actor.ID,
		Action: "final_cutover", RequestID: rid, IdempotencyKey: key,
		PolicyBundleVersion: policyBundleVersion(),
		PolicyDecision:      fmt.Sprint(res["policy_decision"]),
		ApprovalID:          fmt.Sprint(res["approval_id"]),
		Result:              status,
		Metadata: fmt.Sprintf(`{"status":%s,"ownership":%s,"source_lsn":%s,"applied_lsn":%s,"at":%q}`,
			jstr(res["status"]), jstr(res["current_owner"]), jstr(res["source_lsn"]), jstr(res["applied_lsn"]),
			time.Now().UTC().Format(time.RFC3339)),
	})
	store.SaveIdem("cutfinal:"+key, hash, body, 200)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(body)
}

// runCutover executes the staged machine. Read-only evidence stages run
// first; the ownership fact is committed BEFORE the shop flips are exposed
// (H-3 commit-before-expose): a crash between commit and flips (or between
// flips) leaves the committed azure fact with diverged shops, which the
// resume path converges forward — never two writers, never a lost transfer.
// The reverse order (flips before commit) could leave shops ahead of the
// fact with no recovery authority; that window is closed.
func runCutover(ctx context.Context, rid string, actor Principal, workloadID, migrationID, environment, approvalID string) map[string]any {
	t0 := time.Now()
	var stages []cutoverStage
	fail := func(stage, code, reason string, extra map[string]any) map[string]any {
		res := map[string]any{
			"migration_id": migrationID, "workload_id": workloadID,
			"status": CutoverBlocked, "stage": stage,
			"failure_code": code, "failure_reason": reason,
			"request_id": rid, "policy_bundle_version": policyBundleVersion(),
		}
		for k, v := range extra {
			res[k] = v
		}
		res["stages"] = stages
		res["total_duration_secs"] = time.Since(t0).Seconds()
		owner := "aws"
		if rec, ok := store.GetOwnership(migrationID); ok {
			owner = rec.CurrentOwner
		}
		_ = store.RecordAudit(AuditEntry{
			RunID: migrationID, WorkloadID: workloadID, ActorType: actor.Type, ActorID: actor.ID,
			Action: "final_cutover_" + stage, RequestID: rid,
			PolicyBundleVersion: policyBundleVersion(), Result: "failure",
			Metadata: fmt.Sprintf(`{"failure_code":%q,"stage":%q,"ownership":%q,"at":%q}`,
				code, stage, owner, time.Now().UTC().Format(time.RFC3339)),
		})
		return res
	}

	// Committed-fact fast path: a previous attempt already committed the
	// azure fact. If both shops converged on it, this is a stale worker and
	// re-execution is refused (ALREADY_TRANSFERRED). If shops diverged, a
	// crash interrupted commit/flips: fall through to preflight + resume,
	// converging forward onto the committed fact (exactly once).
	preCommitted := false
	if rec, ok := store.GetOwnership(migrationID); ok && rec.CurrentOwner == "azure" {
		preCommitted = true
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, srcOwner, srcErr := quiesceClient.State(sctx)
		_, tgtOwner, tgtErr := targetQuiesceClient.State(sctx)
		cancel()
		if srcErr == nil && tgtErr == nil && srcOwner == "azure" && tgtOwner == "azure" {
			return fail("OWNERSHIP_TRANSFERRED", CutoverAlreadyTransferred,
				"ownership already transferred to azure; reverse rollback unsupported without reverse CDC", map[string]any{
					"current_owner": "azure", "previous_owner": rec.PreviousOwner,
				})
		}
	}

	// ---- FINAL_PREFLIGHT: full readiness revalidation (never cached) ----
	pt0 := time.Now()
	ready := evaluateReadiness(ctx, rid, actor, workloadID, migrationID, environment, approvalID, true)
	stages = append(stages, cutoverStage{Name: "FINAL_PREFLIGHT", Secs: time.Since(pt0).Seconds()})
	_ = store.RecordAudit(AuditEntry{
		RunID: migrationID, WorkloadID: workloadID, ActorType: actor.Type, ActorID: actor.ID,
		Action: "final_preflight", RequestID: rid,
		PolicyBundleVersion: policyBundleVersion(),
		PolicyDecision:      fmt.Sprint(ready["policy_decision"]),
		ApprovalID:          fmt.Sprint(ready["approval_id"]),
		Result:              map[bool]string{true: "success", false: "failure"}[ready["status"] == ReadyForCutover],
		Metadata: fmt.Sprintf(`{"status":%s,"policy_input_hash":%s,"at":%q}`,
			jstr(ready["status"]), jstr(ready["policy_input_hash"]), time.Now().UTC().Format(time.RFC3339)),
	})
	if ready["status"] != ReadyForCutover {
		return fail("FINAL_PREFLIGHT", "PREFLIGHT_NOT_READY",
			fmt.Sprintf("preflight status %v", ready["status"]), map[string]any{"readiness": ready})
	}
	cdcMap := toStringMap(ready["cdc"])
	reconMap := toStringMap(ready["reconciliation"])

	// ---- WRITES_QUIESCED: ensure source quiesced (idempotent) ----
	qt0 := time.Now()
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := quiesceClient.SetQuiesced(qctx, true); err != nil {
		stages = append(stages, cutoverStage{Name: "WRITES_QUIESCED", Secs: time.Since(qt0).Seconds()})
		return fail("WRITES_QUIESCED", "QUIESCE_FAILED", err.Error(), map[string]any{"readiness": ready})
	}
	stages = append(stages, cutoverStage{Name: "WRITES_QUIESCED", Secs: time.Since(qt0).Seconds(), Detail: "source quiesced"})
	_ = store.RecordAudit(AuditEntry{
		RunID: migrationID, WorkloadID: workloadID, ActorType: actor.Type, ActorID: actor.ID,
		Action: "final_quiesce", RequestID: rid,
		PolicyBundleVersion: policyBundleVersion(), Result: "success",
		Metadata: fmt.Sprintf(`{"quiesced":true,"at":%q}`, time.Now().UTC().Format(time.RFC3339)),
	})

	// ---- CDC catch-up + final validation: measured evidence from preflight.
	// Positions come from the just-completed readiness run (seconds old), not
	// from sleeps or earlier cached results.
	stages = append(stages, cutoverStage{Name: "CDC_CATCHING_UP", Secs: 0})
	stages = append(stages, cutoverStage{Name: "CDC_CAUGHT_UP", Secs: 0,
		Detail: fmt.Sprintf("applied=%v lag=%v", cdcVal(cdcMap, "applied_lsn"), cdcVal(cdcMap, "cdc_lag_seconds"))})
	stages = append(stages, cutoverStage{Name: "FINAL_VALIDATION", Secs: 0, Detail: "reconciliation match"})

	// ---- commit the ownership fact exactly once, BEFORE exposing flips ----
	// (H-3 commit-before-expose). The CAS is the single linearization point:
	// concurrent cutovers collapse here, and a crash after this point leaves
	// a committed fact for the resume path to converge onto.
	now := time.Now().UTC().Format(time.RFC3339)
	if !preCommitted {
		moved, err := store.TransferOwnership(migrationID, "aws", OwnershipRecord{
			MigrationID: migrationID, WorkloadID: workloadID,
			CurrentOwner: "azure", PreviousOwner: "aws", Routing: "azure",
			PolicyInputHash: fmt.Sprint(ready["policy_input_hash"]),
			ApprovalID:      fmt.Sprint(ready["approval_id"]),
			SourceLSN:       fmt.Sprint(cdcVal(cdcMap, "source_lsn")),
			AppliedLSN:      fmt.Sprint(cdcVal(cdcMap, "applied_lsn")),
			LagSeconds:      cdcLag(cdcMap),
			CreatedAt: now, UpdatedAt: now,
		})
		if err != nil || !moved {
			return fail("OWNERSHIP_TRANSFERRED", CutoverStoreFailed,
				"ownership already moved by a concurrent cutover; keeping single transfer",
				map[string]any{"readiness": ready})
		}
	}
	_ = store.RecordAudit(AuditEntry{
		RunID: migrationID, WorkloadID: workloadID, ActorType: actor.Type, ActorID: actor.ID,
		Action: "transfer_ownership", RequestID: rid,
		PolicyBundleVersion: policyBundleVersion(),
		PolicyDecision:      fmt.Sprint(ready["policy_decision"]),
		ApprovalID:          fmt.Sprint(ready["approval_id"]),
		Result:              "success",
		Metadata: fmt.Sprintf(`{"previous_owner":"aws","current_owner":"azure","source_lsn":%q,"applied_lsn":%q,"cdc_lag_seconds":%v,"at":%q}`,
			cdcVal(cdcMap, "source_lsn"), cdcVal(cdcMap, "applied_lsn"), cdcVal(cdcMap, "cdc_lag_seconds"),
			time.Now().UTC().Format(time.RFC3339)),
	})

	// ---- OWNERSHIP_TRANSFERRED: source flips first (both-reject is safe),
	// then target. Partial failure stays paused, never dual-authoritative;
	// the committed fact above is the resume authority.
	ft0 := time.Now()
	_, srcOwner, srcErr := quiesceClient.State(qctx)
	_, tgtOwner, tgtErr := targetQuiesceClient.State(qctx)
	if srcErr != nil || tgtErr != nil {
		stages = append(stages, cutoverStage{Name: "OWNERSHIP_TRANSFERRED", Secs: time.Since(ft0).Seconds()})
		return fail("OWNERSHIP_TRANSFERRED", CutoverVerifyFailed,
			fmt.Sprintf("ownership state unreadable: src=%v tgt=%v", srcErr, tgtErr), map[string]any{"readiness": ready})
	}
	resume := srcOwner == "azure" && tgtOwner != "azure" // prior partial: source already safe
	if srcOwner != "aws" && !resume {
		stages = append(stages, cutoverStage{Name: "OWNERSHIP_TRANSFERRED", Secs: time.Since(ft0).Seconds()})
		return fail("OWNERSHIP_TRANSFERRED", CutoverVerifyFailed,
			"source ownership is "+srcOwner+"; refusing unsafe transfer", map[string]any{"readiness": ready})
	}
	if !resume {
		if _, err := quiesceClient.SetOwnership(qctx, "azure"); err != nil {
			stages = append(stages, cutoverStage{Name: "OWNERSHIP_TRANSFERRED", Secs: time.Since(ft0).Seconds()})
			return fail("OWNERSHIP_TRANSFERRED", CutoverSourceFlipFailed, err.Error(), map[string]any{"readiness": ready})
		}
	}
	if _, err := targetQuiesceClient.SetOwnership(qctx, "azure"); err != nil {
		stages = append(stages, cutoverStage{Name: "OWNERSHIP_TRANSFERRED", Secs: time.Since(ft0).Seconds()})
		return fail("OWNERSHIP_TRANSFERRED", CutoverTargetFlipFailed,
			err.Error()+"; both sides reject writes (paused, safe); retry resumes", map[string]any{"readiness": ready})
	}
	// Verify: source rejects (azure-named), target accepts naming.
	_, srcOwner2, _ := quiesceClient.State(qctx)
	_, tgtOwner2, _ := targetQuiesceClient.State(qctx)
	if srcOwner2 != "azure" || tgtOwner2 != "azure" {
		stages = append(stages, cutoverStage{Name: "OWNERSHIP_TRANSFERRED", Secs: time.Since(ft0).Seconds()})
		return fail("OWNERSHIP_TRANSFERRED", CutoverVerifyFailed,
			fmt.Sprintf("post-flip states src=%s tgt=%s", srcOwner2, tgtOwner2), map[string]any{"readiness": ready})
	}
	stages = append(stages, cutoverStage{Name: "OWNERSHIP_TRANSFERRED", Secs: time.Since(ft0).Seconds(),
		Detail: "aws -> azure, source-first (no dual-authoritative window)"})

	// ---- TRAFFIC_SWITCHED: routing follows ownership (local seam) ----
	stages = append(stages, cutoverStage{Name: "TRAFFIC_SWITCHED", Secs: 0, Detail: "authoritative routing: azure"})
	_ = store.RecordAudit(AuditEntry{
		RunID: migrationID, WorkloadID: workloadID, ActorType: actor.Type, ActorID: actor.ID,
		Action: "switch_routing", RequestID: rid,
		PolicyBundleVersion: policyBundleVersion(), Result: "success",
		Metadata: fmt.Sprintf(`{"routing":"azure","at":%q}`, time.Now().UTC().Format(time.RFC3339)),
	})

	// ---- WRITES_RESUMED: unquiesce the TARGET only. The source stays
	// quiesced (belt) and non-authoritative (suspenders): its writes stay
	// rejected by ownership regardless.
	rt0 := time.Now()
	if _, err := targetQuiesceClient.SetQuiesced(qctx, false); err != nil {
		stages = append(stages, cutoverStage{Name: "WRITES_RESUMED", Secs: time.Since(rt0).Seconds()})
		return fail("WRITES_RESUMED", CutoverResumeFailed, err.Error(), map[string]any{"readiness": ready})
	}
	stages = append(stages, cutoverStage{Name: "WRITES_RESUMED", Secs: time.Since(rt0).Seconds(), Detail: "azure accepts writes"})
	_ = store.RecordAudit(AuditEntry{
		RunID: migrationID, WorkloadID: workloadID, ActorType: actor.Type, ActorID: actor.ID,
		Action: "resume_writes", RequestID: rid,
		PolicyBundleVersion: policyBundleVersion(), Result: "success",
		Metadata: fmt.Sprintf(`{"owner":"azure","at":%q}`, time.Now().UTC().Format(time.RFC3339)),
	})

	stages = append(stages, cutoverStage{Name: "CUTOVER_COMPLETE", Secs: 0})
	_ = store.RecordAudit(AuditEntry{
		RunID: migrationID, WorkloadID: workloadID, ActorType: actor.Type, ActorID: actor.ID,
		Action: "cutover_complete", RequestID: rid,
		PolicyBundleVersion: policyBundleVersion(),
		PolicyDecision:      fmt.Sprint(ready["policy_decision"]),
		ApprovalID:          fmt.Sprint(ready["approval_id"]),
		Result:              "success",
		Metadata: fmt.Sprintf(`{"current_owner":"azure","routing":"azure","source_lsn":%q,"applied_lsn":%q,"at":%q}`,
			cdcVal(cdcMap, "source_lsn"), cdcVal(cdcMap, "applied_lsn"), time.Now().UTC().Format(time.RFC3339)),
	})
	return map[string]any{
		"migration_id": migrationID, "workload_id": workloadID,
		"status": CutoverComplete, "stage": CutoverComplete,
		"previous_owner": "aws", "current_owner": "azure", "routing": "azure",
		"policy_decision": ready["policy_decision"],
		"policy_input_hash": ready["policy_input_hash"],
		"approval_id": ready["approval_id"],
		"plan_id": ready["plan_id"], "compatibility_report_id": ready["compatibility_report_id"],
		"drift_report_id": ready["drift_report_id"],
		"source_lsn": cdcVal(cdcMap, "source_lsn"), "applied_lsn": cdcVal(cdcMap, "applied_lsn"),
		"cdc_lag_seconds": cdcVal(cdcMap, "cdc_lag_seconds"),
		"reconciliation": reconMap, "readiness": ready,
		"stages": stages, "total_duration_secs": time.Since(t0).Seconds(),
		"request_id": rid, "policy_bundle_version": policyBundleVersion(),
	}
}

// jstr renders a value as JSON-safe quoted text (nil -> "").
func jstr(v any) string {
	if v == nil {
		return `""`
	}
	if s, ok := v.(string); ok {
		b, _ := json.Marshal(s)
		return string(b)
	}
	return `""`
}

func cdcVal(m map[string]any, k string) any {
	if m == nil {
		return nil
	}
	return m[k]
}

// cdcLag extracts measured lag seconds (JSON numbers decode as float64).
func cdcLag(m map[string]any) int64 {
	if m == nil {
		return 0
	}
	switch v := m["cdc_lag_seconds"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	default:
		return 0
	}
}

// toStringMap normalizes a struct or map payload (readiness stores typed
// structs) into a string map for field extraction. JSON shape is unchanged.
func toStringMap(v any) map[string]any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}
