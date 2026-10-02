// Cutover readiness: the READY_FOR_CUTOVER boundary without ownership
// transfer. Recomputed fresh on every call from current evidence (never a
// stored decision, so material changes invalidate it by construction).
//
// Required (WORKFLOW_STATE_MACHINE.md cutover gate, staged for this slice):
// current compat + plan + clear drift, policy allow at the weight-0 baseline,
// approval where required, canary PASS at 1/5/25/50, AWS authoritative,
// writes quiesced, CDC caught up (applied >= base, lag within the
// workload-spec RPO, measured), target reconciliation match. This state
// transfers nothing.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	cdc "skybridge/cdc-applier"
)

// Readiness statuses.
const (
	ReadyForCutover = "READY_FOR_CUTOVER"
	NotReady        = "NOT_READY"
)

// ReadinessCheck is one named gate outcome.
type ReadinessCheck struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// postReadinessForCutover implements POST /v1/migrations/{id}/readiness.
// The policy baseline is always weight 0 (no traffic shifted yet); any other
// target_weight is rejected. Same strict idempotency contract as siblings.
func postReadinessForCutover(w http.ResponseWriter, r *http.Request, workloadID, migrationID string) {
	rid := reqID(r)
	actor, ok := requireAuth(w, r)
	if !ok {
		return
	}
	mig, ok := store.GetMigration(migrationID)
	if !ok {
		writeErr(w, rid, "NOT_FOUND", "migration not found", 404)
		return
	}
	if wid, _ := mig["workload_id"].(string); wid != workloadID {
		writeErr(w, rid, "NOT_FOUND", "migration does not belong to workload", 404)
		return
	}
	if !requireWorkloadAccess(w, r, actor, workloadID) {
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
		TargetWeight int   `json:"target_weight"`
		ApprovalID   string `json:"approval_id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "invalid JSON", 400)
		return
	}
	if in.Environment == "" {
		in.Environment = "dev"
	}
	if in.TargetWeight != 0 {
		writeErr(w, rid, "VALIDATION_FAILED", "readiness evaluates the weight-0 baseline only", 400)
		return
	}
	mu := idemLock("cutready:" + key)
	mu.Lock()
	defer mu.Unlock()
	if resp, status, storedHash, found := store.CheckIdem("cutready:" + key); found {
		if storedHash == "" || storedHash == hash {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(resp)
			return
		}
		idempotencyConflict(w, actor, "evaluate_cutover_readiness", key, rid, AuditEntry{WorkloadID: workloadID, RunID: migrationID})
		return
	}
	res := evaluateReadiness(r.Context(), rid, actor, workloadID, migrationID, in.Environment, in.ApprovalID, false)
	body, _ := json.Marshal(res)
	result := "success"
	if res["status"] != ReadyForCutover {
		result = "denied"
	}
	_ = store.RecordAudit(AuditEntry{
		RunID: migrationID, WorkloadID: workloadID, ActorType: actor.Type, ActorID: actor.ID,
		Action: "evaluate_cutover_readiness", RequestID: rid, IdempotencyKey: key,
		PolicyBundleVersion: policyBundleVersion(),
		PolicyDecision:      fmt.Sprint(res["policy_decision"]),
		ApprovalID:          fmt.Sprint(res["approval_id"]),
		Result:              result,
		Metadata:            fmt.Sprintf(`{"status":%q}`, res["status"]),
	})
	store.SaveIdem("cutready:"+key, hash, body, 200)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(body)
}

func readinessCheck(name string, pass bool, detail string) ReadinessCheck {
	return ReadinessCheck{Name: name, Pass: pass, Detail: detail}
}

// evaluateReadiness runs every gate against live evidence. allPass is true
// only when every check passes; ownership is observed, never mutated.
// allowResumePartial admits one paused-safe state: no azure ownership record,
// source admin already azure, target admin not yet azure (a prior transfer
// stopped between flips; both sides reject writes). The policy input still
// carries intent aws; only the observation is tolerated, and only here.
// Actor identity is the verified authentication principal.
func evaluateReadiness(ctx context.Context, rid string, actor Principal, workloadID, migrationID, environment, approvalID string, allowResumePartial bool) map[string]any {
	t0 := time.Now()
	checks := []ReadinessCheck{}
	fail := func(name, detail string) {
		checks = append(checks, readinessCheck(name, false, detail))
	}
	pass := func(name, detail string) {
		checks = append(checks, readinessCheck(name, true, detail))
	}
	res := map[string]any{
		"migration_id": migrationID, "workload_id": workloadID,
		"status": NotReady, "request_id": rid,
		"policy_bundle_version": policyBundleVersion(),
	}

	// 1. evidence freshness (measurement-independent).
	provisional := map[string]any{
		"action": "shift_traffic", "environment": environment,
		"target_weight": 0, "read_only_canary": true,
		"write_ownership": "aws", "validation_status": "passed",
		"cdc_lag_seconds": 0, "target_healthy": true,
	}
	pin, efail := buildPolicyInput(workloadID, provisional, actor)
	if efail != nil {
		fail("evidence_fresh", efail.Code+": "+efail.Message)
		return finishReadiness(res, checks, t0)
	}
	// RPO comes from the workload spec via the policy input (same source
	// the policy freshness gate uses); never a hardcoded constant, so a
	// non-default rpo_seconds cannot pass policy yet fail readiness.
	rpoSeconds := pin.RPOSeconds
	if rpoSeconds <= 0 {
		rpoSeconds = 30
	}
	plan, _ := store.GetLatestTargetPlan(workloadID)
	rep, _ := store.GetLatestCompatReport(workloadID)
	drift, _ := store.GetLatestDriftReport(workloadID, plan.ID)
	pass("evidence_fresh", "compat+plan+drift current")
	res["plan_id"], res["compatibility_report_id"], res["drift_report_id"] = plan.ID, rep.ID, drift.ID

	// 2. canary progression.
	if canaryPassForStages(migrationID, []int{1, 5, 25, 50}) {
		if latest, ok := latestCanary(migrationID); ok && latest.Verdict == CanaryPass {
			pass("canary", "stages 1/5/25/50 PASS, latest PASS")
		} else {
			fail("canary", "latest stage verdict is not PASS")
			return finishReadiness(res, checks, t0)
		}
	} else {
		fail("canary", "stages 1/5/25/50 require PASS records")
		return finishReadiness(res, checks, t0)
	}

	// 3. quiesce + ownership (observed only).
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	quiesced, ownership, err := quiesceClient.State(qctx)
	if err != nil {
		fail("quiesce", "quiesce state unavailable: "+err.Error())
		return finishReadiness(res, checks, t0)
	}
	if !quiesced {
		fail("quiesce", "writes not quiesced")
		return finishReadiness(res, checks, t0)
	}
	pass("quiesce", "writes quiesced")
	if ownership != "aws" {
		resumed := false
		if allowResumePartial {
			if _, ok := store.GetOwnership(migrationID); !ok {
				rctx, rcancel := context.WithTimeout(ctx, 20*time.Second)
				_, tgtOwner, terr := targetQuiesceClient.State(rctx)
				rcancel()
				if terr == nil && tgtOwner != "azure" && ownership == "azure" {
					resumed = true
				}
			} else {
				// H-3 commit-before-expose: the azure ownership fact is
				// already committed but the shops have not converged on it
				// (crash between commit and flips, or between flips).
				// Resume converges forward onto the committed fact:
				// paused-safe, never dual-authoritative. (A fully converged
				// transfer never reaches here: runCutover refuses stale
				// workers with ALREADY_TRANSFERRED before preflight.)
				resumed = true
			}
		}
		if !resumed {
			fail("ownership", "write ownership is "+ownership+", must stay aws in this slice")
			return finishReadiness(res, checks, t0)
		}
		pass("ownership", "resume partial transfer: source safe, target pending")
		res["write_ownership"] = ownership
		res["resume_partial"] = true
	} else {
		pass("ownership", "aws authoritative")
		res["write_ownership"] = ownership
	}

	// 4. CDC catch-up (measured): healthy, applied >= base, lag within RPO.
	cctx, cancel2 := context.WithTimeout(ctx, 150*time.Second)
	defer cancel2()
	probeID := uuidFromString(migrationID + "|readiness|" + rid)
	cdcRep, err := rehearsalDataPlane.RunReplication(cctx, RehearsalSpec{
		RehearsalKey: "readiness:" + rid, ProbeIDs: []string{probeID},
		ProbeCount: 1, Timeout: 120 * time.Second,
		RPOSeconds: pin.RPOSeconds,
	})
	if err != nil {
		fail("cdc_catchup", "replication unavailable: "+err.Error())
		return finishReadiness(res, checks, t0)
	}
	res["cdc"] = cdcRep
	if !cdcRep.TargetHealthy {
		fail("cdc_catchup", "target unhealthy")
		return finishReadiness(res, checks, t0)
	}
	caughtUp := false
	if cdcRep.BaseLSN != "" && cdcRep.AppliedLSN != "" {
		if cmp, cerr := cdc.CompareLSN(cdcRep.AppliedLSN, cdcRep.BaseLSN); cerr == nil && cmp >= 0 {
			caughtUp = true
		}
	}
	lag := cdcRep.LagSeconds
	if lag < 0 {
		lag = 0
	}
	if !caughtUp {
		fail("cdc_catchup", "applied position behind pre-probe base")
		return finishReadiness(res, checks, t0)
	}
	if lag > int64(rpoSeconds) {
		fail("cdc_catchup", fmt.Sprintf("lag %ds exceeds RPO %ds", lag, rpoSeconds))
		return finishReadiness(res, checks, t0)
	}
	pass("cdc_catchup", fmt.Sprintf("applied=%s lag=%ds", cdcRep.AppliedLSN, lag))
	res["rpo_decision"] = "within_rpo"

	// 5. reconciliation over the readiness probe.
	recon, err := rehearsalDataPlane.Reconcile(cctx, cdcRep.ProbeIDs)
	if err != nil {
		fail("reconciliation", "reconciliation unavailable: "+err.Error())
		return finishReadiness(res, checks, t0)
	}
	res["reconciliation"] = recon
	if !recon.Match {
		fail("reconciliation", "source/target diverge")
		return finishReadiness(res, checks, t0)
	}
	pass("reconciliation", "probe set converges")

	// 6. final policy at the weight-0 baseline with measured evidence.
	finalRuntime := map[string]any{
		"action": "shift_traffic", "environment": environment,
		"target_weight": 0, "read_only_canary": true,
		"write_ownership": "aws", "validation_status": "passed",
		"cdc_lag_seconds": int(lag), "target_healthy": true,
	}
	finalPin, efail := buildPolicyInput(workloadID, finalRuntime, actor)
	if efail != nil {
		fail("policy", efail.Code+": "+efail.Message)
		return finishReadiness(res, checks, t0)
	}
	decision, reasons := policyDecide(finalPin)
	res["policy_decision"] = decision
	res["policy_input_hash"] = finalPin.inputHash()
	if decision == PolicyDeny {
		fail("policy", strings.Join(reasons, "; "))
		return finishReadiness(res, checks, t0)
	}
	if decision == PolicyApprovalRequired {
		appr, code, msg := approvalEligibleForUse(workloadID, approvalID, finalPin)
		if appr == nil {
			fail("approval", code+": "+msg)
			return finishReadiness(res, checks, t0)
		}
		res["approval_id"] = appr.ID
		pass("approval", "eligible approval "+appr.ID)
	} else {
		pass("policy", "allow")
	}

	res["status"] = ReadyForCutover
	return finishReadiness(res, checks, t0)
}

func finishReadiness(res map[string]any, checks []ReadinessCheck, t0 time.Time) map[string]any {
	res["checks"] = checks
	if res["status"] != ReadyForCutover {
		res["status"] = NotReady
	}
	elapsed := time.Since(t0).Seconds()
	res["stage_durations_secs"] = map[string]float64{"evaluate_readiness_secs": elapsed}
	res["total_duration_secs"] = elapsed
	return res
}
