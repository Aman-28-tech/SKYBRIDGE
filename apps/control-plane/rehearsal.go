// Migration rehearsal: deterministic end-to-end dry run that exercises the
// complete control/data-plane path and stops at a proven READY boundary.
//
// Rehearsal reuses the Migration/MigrationRun model — it creates no migration
// resources, no executions, no workflows. It persists nothing except the
// strict-semantic idempotency record and audit events (existing systems).
//
// Stage order (each timed and audited; first failure stops later stages):
//
//	accepted -> validate_input -> evidence -> provisional_policy
//	  -> provisioning_tfvars -> replication -> reconciliation
//	  -> final_policy -> provisioning_intent -> REHEARSAL_READY
//
// Terminal statuses: REHEARSAL_READY | REHEARSAL_BLOCKED. HTTP 200 carries
// both (a block is a decision, like readiness); 4xx only for malformed
// input, missing/stale evidence, or idempotency conflicts.
//
// What rehearsal NEVER does: write quiesce, ownership transfer, traffic
// shift, cutover, rollback, reverse CDC, cloud mutation, Terraform apply.
// Apply is never called anywhere in this file (grep for RequestApply: absent).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Rehearsal terminal statuses.
const (
	RehearsalReady   = "REHEARSAL_READY"
	RehearsalBlocked = "REHEARSAL_BLOCKED"
)

// Rehearsal failure codes. Staleness codes reuse the policy gate vocabulary;
// data-plane codes are rehearsal-scoped.
const (
	RehearsalApprovalRequired = "APPROVAL_REQUIRED"
	RehearsalTargetUnhealthy  = "TARGET_UNHEALTHY"
	RehearsalCDCUnavailable   = "CDC_UNAVAILABLE"
	RehearsalCaptureTimeout   = "CDC_CAPTURE_TIMEOUT"
	RehearsalReconMismatch    = "RECONCILIATION_MISMATCH"
	RehearsalAdapterFailed    = "ADAPTER_VALIDATION_FAILED"
)

// RehearsalRequest is the POST body. Environment/weight default to the
// no-traffic baseline (dev/0); approval binds the conditional path.
type RehearsalRequest struct {
	Environment string `json:"environment"`
	TargetWeight int   `json:"target_weight"`
	ApprovalID   string `json:"approval_id,omitempty"`
	Probes       int    `json:"probes,omitempty"`
	TimeoutSecs  int    `json:"timeout_secs,omitempty"`
}

func (r RehearsalRequest) withDefaults() RehearsalRequest {
	if r.Environment == "" {
		r.Environment = "dev"
	}
	// TargetWeight 0 is the zero value and the rehearsal baseline; only
	// reject explicitly non-canonical weights.
	if r.Probes <= 0 {
		r.Probes = 2
	}
	if r.TimeoutSecs <= 0 {
		r.TimeoutSecs = 120
	}
	return r
}

// CDCReport is the measured replication state for one rehearsal. Positions
// and lag are measured, never asserted from fixtures. BaseLSN is the source
// position before probe writes (catch-up: applied must reach it); ProbeIDs
// identifies the applied probe set for reconciliation.
type CDCReport struct {
	SourceLSN       string `json:"source_lsn"`
	AppliedLSN      string `json:"applied_lsn"`
	BaseLSN         string `json:"base_lsn,omitempty"`
	ProbeIDs        []string `json:"probe_ids,omitempty"`
	SourceCommitUnix int64 `json:"source_commit_unix"`
	ObserveUnix      int64 `json:"observe_unix"`
	LagSeconds       int64 `json:"cdc_lag_seconds"`
	WithinRPO        bool  `json:"within_rpo"`
	EventsCaptured   uint64 `json:"events_captured"`
	EventsApplied    uint64 `json:"events_applied"`
	EventsDuplicates uint64 `json:"events_duplicates"`
	CheckpointLSN    string `json:"checkpoint_lsn"`
	TargetHealthy    bool  `json:"target_healthy"`
}

// TableRecon is the per-table reconciliation (IDs and field names only;
// values/PII are never reported).
type TableRecon struct {
	Table          string   `json:"table"`
	SourceCount    int      `json:"source_count"`
	TargetCount    int      `json:"target_count"`
	ProbesExpected int      `json:"probes_expected"`
	ProbesMatched  int      `json:"probes_matched"`
	MissingIDs     []string `json:"missing_ids"`
	UnexpectedIDs  []string `json:"unexpected_ids"`
	Mismatched     []string `json:"mismatched_ids"`
}

// Reconciliation is the deterministic source-vs-target comparison over the
// rehearsal probe set (plus counts for context).
type Reconciliation struct {
	Tables      []TableRecon `json:"tables"`
	Match       bool         `json:"match"`
	Fingerprint string       `json:"fingerprint"`
}

// RehearsalResult is the structured result contract (§8). Execution/workflow
// IDs are empty by design: rehearsal starts no Temporal execution and stops
// before cutover.
type RehearsalResult struct {
	RehearsalID           string            `json:"rehearsal_id"`
	WorkloadID            string            `json:"workload_id"`
	MigrationID           string            `json:"migration_id"`
	Stage                 string            `json:"stage"`
	Status                string            `json:"status"`
	PolicyDecision        string            `json:"policy_decision,omitempty"`
	ApprovalID            string            `json:"approval_id,omitempty"`
	PlanID                string            `json:"plan_id,omitempty"`
	CompatibilityReportID string            `json:"compatibility_report_id,omitempty"`
	DriftReportID         string            `json:"drift_report_id,omitempty"`
	PolicyInputHash       string            `json:"policy_input_hash,omitempty"`
	PolicyBundleVersion   string            `json:"policy_bundle_version"`
	CDC                   *CDCReport        `json:"cdc,omitempty"`
	Reconciliation        *Reconciliation   `json:"reconciliation,omitempty"`
	RPODecision           string            `json:"rpo_decision,omitempty"`
	ProvisioningProvider  string            `json:"adapter_provider,omitempty"`
	ProvisioningOpKey     string            `json:"operation_key,omitempty"`
	ProvisioningFingerprint string          `json:"provisioning_fingerprint,omitempty"`
	TerraformFingerprint  string            `json:"terraform_fingerprint,omitempty"`
	ExecutionID           string            `json:"execution_id,omitempty"`
	WorkflowID            string            `json:"workflow_id,omitempty"`
	RunID                 string            `json:"run_id,omitempty"`
	FailureCode           string            `json:"failure_code,omitempty"`
	FailureReason         string            `json:"failure_reason,omitempty"`
	Stages                []string          `json:"stages_completed"`
	StageDurationsSecs    map[string]float64 `json:"stage_durations_secs"`
	TotalDurationSecs     float64           `json:"total_duration_secs"`
}

// RehearsalSpec is the frozen input the data plane executes.
type RehearsalSpec struct {
	RehearsalKey string
	ProbeIDs     []string
	ProbeCount   int
	Timeout      time.Duration
	// RPOSeconds carries the workload-spec RPO so WithinRPO is evaluated
	// against the same threshold the policy gate uses. Zero means the
	// historical default of 30 (keeps existing fakes/tests unchanged).
	RPOSeconds int
}

// RehearsalDataPlane is the seam between orchestration and the local CDC
// path. The live implementation measures PG + Pandaproxy; tests inject
// scripted fakes. No cloud, no Terraform behind this interface.
type RehearsalDataPlane interface {
	RunReplication(ctx context.Context, spec RehearsalSpec) (CDCReport, error)
	Reconcile(ctx context.Context, probeIDs []string) (Reconciliation, error)
}

// rehearsalDataPlane is the process seam (live by default, fakes in tests),
// mirroring the temporalClient pattern.
var rehearsalDataPlane RehearsalDataPlane = liveRehearsalDataPlane()

// uuidFromString derives a deterministic UUID from arbitrary input
// (probes must be stable across re-execution of the same rehearsal key).
func uuidFromString(s string) string {
	sum := sha256.Sum256([]byte(s))
	h := hex.EncodeToString(sum[:])
	return fmt.Sprintf("%s-%s-4%s-%s-%s", h[0:8], h[8:12], h[13:16], h[16:20], h[20:32])
}

// rehearsalID deterministically identifies a rehearsal from its evidence and
// measured positions. Same inputs -> same ID; live positions differ per run.
func rehearsalID(workloadID, migrationID, planID, compatID, driftID, approvalID string, cdc *CDCReport, recon *Reconciliation) string {
	src, applied, fp := "", "", ""
	if cdc != nil {
		src, applied = cdc.SourceLSN, cdc.AppliedLSN
	}
	if recon != nil {
		fp = recon.Fingerprint
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		workloadID, migrationID, planID, compatID, driftID, approvalID, src, applied, fp,
	}, "|")))
	return "rehearsal-" + hex.EncodeToString(sum[:])[:16]
}

type rehearsalEngine struct {
	rid         string
	key         string
	actor       Principal
	migrationID string
	workloadID  string
	req         RehearsalRequest
	started     time.Time
	stages      []string
	durations   map[string]float64
}

func (e *rehearsalEngine) completeStage(name string, at time.Time) {
	e.stages = append(e.stages, name)
	e.durations[name] = time.Since(at).Seconds()
}

func (e *rehearsalEngine) audit(action, result, approvalID, policyDecision, meta string) {
	_ = store.RecordAudit(AuditEntry{
		RunID: e.migrationID, WorkloadID: e.workloadID,
		ActorType: e.actor.Type, ActorID: e.actor.ID, Action: action,
		RequestID: e.rid, IdempotencyKey: e.key,
		PolicyBundleVersion: policyBundleVersion(), PolicyDecision: policyDecision,
		ApprovalID: approvalID, Result: result, Metadata: meta,
	})
}

func (e *rehearsalEngine) blocked(res *RehearsalResult, stage, code, reason string) *RehearsalResult {
	res.Stage = stage
	res.Status = RehearsalBlocked
	res.FailureCode = code
	res.FailureReason = reason
	e.audit("rehearsal_"+stage, "failure", res.ApprovalID, res.PolicyDecision,
		fmt.Sprintf(`{"rehearsal_id":%q,"failure_code":%q}`, res.RehearsalID, code))
	return res
}

// run executes the stage machine. It returns the result; callers persist the
// idempotency record and audit completion.
func (e *rehearsalEngine) run(ctx context.Context) *RehearsalResult {
	res := &RehearsalResult{
		WorkloadID: e.workloadID, MigrationID: e.migrationID,
		Stage: "accepted", Status: RehearsalBlocked,
		PolicyBundleVersion: policyBundleVersion(),
		Stages: []string{}, StageDurationsSecs: map[string]float64{},
	}
	mig, ok := store.GetMigration(e.migrationID)
	if !ok {
		return e.blocked(res, "accepted", "NOT_FOUND", "migration not found")
	}
	if wid, _ := mig["workload_id"].(string); wid != e.workloadID {
		return e.blocked(res, "accepted", "NOT_FOUND", "migration does not belong to workload")
	}
	wl, ok := store.GetWorkload(e.workloadID)
	if !ok {
		return e.blocked(res, "accepted", "NOT_FOUND", "workload not found")
	}
	name, _ := wl["name"].(string)
	t0 := time.Now()
	e.completeStage("accepted", t0)
	e.audit("rehearsal_start", "success", "", "", fmt.Sprintf(`{"environment":%q,"target_weight":%d}`, e.req.Environment, e.req.TargetWeight))

	// ---- evidence (freshness only; measurement-independent) ----
	t := time.Now()
	provisionalRuntime := map[string]any{
		"action": "shift_traffic", "environment": e.req.Environment,
		"target_weight": e.req.TargetWeight, "read_only_canary": true,
		"write_ownership": "aws", "validation_status": "passed",
		"cdc_lag_seconds": 0, "target_healthy": true,
	}
	pin, fail := buildPolicyInput(e.workloadID, provisionalRuntime, e.actor)
	if fail != nil {
		e.completeStage("evidence", t)
		return e.blocked(res, "evidence", fail.Code, fail.Message)
	}
	plan, _ := store.GetLatestTargetPlan(e.workloadID)
	rep, _ := store.GetLatestCompatReport(e.workloadID)
	drift, _ := store.GetLatestDriftReport(e.workloadID, plan.ID)
	res.PlanID, res.CompatibilityReportID, res.DriftReportID = plan.ID, rep.ID, drift.ID
	e.completeStage("evidence", t)

	// ---- provisional policy: compat/drift/config verdicts are final here;
	// lag/health/validation verdicts are re-evaluated after measurement. ----
	t = time.Now()
	provisionalDecision, reasons := policyDecide(pin)
	terminal := func() (string, bool) {
		for _, r := range reasons {
			switch r {
			case "compatibility_not_pass", "blocking_drift", "security_critical_drift",
				"unsupported_action", "non_canonical_weight", "denied",
				"write_ownership_violation", "read_only_canary_violation",
				"ownership_transfer_not_verified":
				return r, true
			}
		}
		return "", false
	}
	if reason, isTerminal := terminal(); isTerminal {
		e.completeStage("provisional_policy", t)
		res.PolicyDecision = PolicyDeny
		res.PolicyInputHash = pin.inputHash()
		return e.blocked(res, "provisional_policy", reason, reason)
	}
	e.completeStage("provisional_policy", t)
	e.audit("rehearsal_policy_provisional", "success", "", provisionalDecision,
		fmt.Sprintf(`{"policy_input_hash":%q}`, pin.inputHash()))

	// ---- provisioning tfvars fingerprint (pure plan consumption; no auth,
	// no cloud, no apply). Blocked/unknown plans refuse here. ----
	t = time.Now()
	tfDoc, err := GenerateTerraformPlan(plan, "azure", name)
	if err != nil {
		e.completeStage("provisioning_tfvars", t)
		if ae, ok := err.(*AdapterError); ok {
			return e.blocked(res, "provisioning_tfvars", RehearsalAdapterFailed, ae.Code+": "+ae.Message)
		}
		return e.blocked(res, "provisioning_tfvars", RehearsalAdapterFailed, err.Error())
	}
	res.TerraformFingerprint = tfDoc.Fingerprint
	e.completeStage("provisioning_tfvars", t)
	e.audit("rehearsal_provisioning_tfvars", "success", "", "",
		fmt.Sprintf(`{"terraform_fingerprint":%q,"operation_key":%q}`, tfDoc.Fingerprint, tfDoc.OperationKey))

	// ---- replication (measured CDC path) ----
	t = time.Now()
	probeIDs := make([]string, 0, e.req.Probes)
	for i := 0; i < e.req.Probes; i++ {
		probeIDs = append(probeIDs, uuidFromString(e.key+"|probe|"+fmt.Sprint(i)))
	}
	cdcRep, err := rehearsalDataPlane.RunReplication(ctx, RehearsalSpec{
		RehearsalKey: e.key, ProbeIDs: probeIDs,
		ProbeCount: e.req.Probes, Timeout: time.Duration(e.req.TimeoutSecs) * time.Second,
		RPOSeconds: pin.RPOSeconds,
	})
	e.completeStage("replication", t)
	if err != nil {
		return e.blocked(res, "replication", RehearsalCDCUnavailable, err.Error())
	}
	res.CDC = &cdcRep
	if !cdcRep.TargetHealthy {
		res.PolicyDecision = PolicyDeny
		return e.blocked(res, "replication", RehearsalTargetUnhealthy, "target database unreachable during rehearsal")
	}
	e.audit("rehearsal_replication", "success", "", "",
		fmt.Sprintf(`{"source_lsn":%q,"applied_lsn":%q,"cdc_lag_seconds":%d,"events_captured":%d,"events_applied":%d,"events_duplicates":%d}`,
			cdcRep.SourceLSN, cdcRep.AppliedLSN, cdcRep.LagSeconds,
			cdcRep.EventsCaptured, cdcRep.EventsApplied, cdcRep.EventsDuplicates))

	// ---- reconciliation (measured validation) ----
	t = time.Now()
	recon, err := rehearsalDataPlane.Reconcile(ctx, probeIDs)
	e.completeStage("reconciliation", t)
	if err != nil {
		return e.blocked(res, "reconciliation", RehearsalCDCUnavailable, err.Error())
	}
	res.Reconciliation = &recon
	validationStatus := "passed"
	if !recon.Match {
		validationStatus = "failed"
	}
	e.audit("rehearsal_validation", map[bool]string{true: "success", false: "failure"}[recon.Match], "", "",
		fmt.Sprintf(`{"reconciliation_fingerprint":%q,"match":%v}`, recon.Fingerprint, recon.Match))

	// ---- final policy with measured evidence ----
	t = time.Now()
	finalRuntime := map[string]any{
		"action": "shift_traffic", "environment": e.req.Environment,
		"target_weight": e.req.TargetWeight, "read_only_canary": true,
		"write_ownership": "aws", "validation_status": validationStatus,
		"cdc_lag_seconds": int(cdcRep.LagSeconds), "target_healthy": cdcRep.TargetHealthy,
	}
	finalPin, fail := buildPolicyInput(e.workloadID, finalRuntime, e.actor)
	if fail != nil {
		e.completeStage("final_policy", t)
		return e.blocked(res, "final_policy", fail.Code, fail.Message)
	}
	decision, reasons := policyDecide(finalPin)
	res.PolicyDecision = decision
	res.PolicyInputHash = finalPin.inputHash()
	lag := cdcRep.LagSeconds
	if lag < 0 {
		lag = 0
	}
	// Same RPO source as the policy freshness gate (finalPin), so the
	// rehearsal RPO verdict can never contradict the policy decision.
	rpoSeconds := finalPin.RPOSeconds
	if rpoSeconds <= 0 {
		rpoSeconds = 30
	}
	res.RPODecision = map[bool]string{true: "within_rpo", false: "rpo_breach"}[lag <= int64(rpoSeconds)]
	e.completeStage("final_policy", t)
	e.audit("rehearsal_policy", map[bool]string{true: "success", false: "denied"}[decision != PolicyDeny],
		"", decision, fmt.Sprintf(`{"policy_input_hash":%q}`, finalPin.inputHash()))
	if decision == PolicyDeny {
		reason := strings.Join(reasons, "; ")
		code := reason
		if len(reasons) > 0 {
			code = reasons[0]
		}
		if validationStatus == "failed" {
			code = RehearsalReconMismatch
		}
		return e.blocked(res, "final_policy", code, reason)
	}
	if decision == PolicyApprovalRequired {
		appr, code, msg := approvalEligibleForUse(e.workloadID, e.req.ApprovalID, finalPin)
		if appr == nil {
			e.audit("rehearsal_approval", "denied", e.req.ApprovalID, decision, "")
			if code == "APPROVAL_REQUIRED" {
				code = RehearsalApprovalRequired
			}
			return e.blocked(res, "final_policy", code, msg)
		}
		res.ApprovalID = appr.ID
		e.audit("rehearsal_approval", "success", appr.ID, decision,
			fmt.Sprintf(`{"policy_input_hash":%q}`, finalPin.inputHash()))
	}

	// ---- provisioning intent (authorized with the final, measured hash) ----
	t = time.Now()
	adapter, err := SelectAdapter("azure")
	if err != nil {
		e.completeStage("provisioning_intent", t)
		return e.blocked(res, "provisioning_intent", RehearsalAdapterFailed, err.Error())
	}
	preq := NewProvisioningRequest(plan, "azure", e.workloadID, e.migrationID,
		"", finalPin.inputHash(), res.ApprovalID)
	preq.Authorized = true
	if err := adapter.ValidateTarget(ctx, preq); err != nil {
		e.completeStage("provisioning_intent", t)
		return e.blocked(res, "provisioning_intent", RehearsalAdapterFailed, err.Error())
	}
	pplan, err := adapter.PlanInfrastructure(ctx, preq)
	if err != nil {
		e.completeStage("provisioning_intent", t)
		return e.blocked(res, "provisioning_intent", RehearsalAdapterFailed, err.Error())
	}
	vres, err := adapter.VerifyInfrastructure(ctx, pplan)
	if err != nil || !vres.Verified {
		e.completeStage("provisioning_intent", t)
		msg := "verification failed"
		if err != nil {
			msg = err.Error()
		}
		return e.blocked(res, "provisioning_intent", RehearsalAdapterFailed, msg)
	}
	res.ProvisioningProvider = "azure"
	res.ProvisioningOpKey = pplan.OperationKey
	res.ProvisioningFingerprint = pplan.Fingerprint
	e.completeStage("provisioning_intent", t)
	e.audit("rehearsal_provisioning_intent", "success", res.ApprovalID, decision,
		fmt.Sprintf(`{"provider":"azure","operation_key":%q,"fingerprint":%q,"policy_input_hash":%q}`,
			pplan.OperationKey, pplan.Fingerprint, finalPin.inputHash()))

	// ---- ready: no executions, no cutover, no ownership change ----
	res.RehearsalID = rehearsalID(e.workloadID, e.migrationID, plan.ID, rep.ID, drift.ID, res.ApprovalID, res.CDC, res.Reconciliation)
	res.Stage = RehearsalReady
	res.Status = RehearsalReady
	res.Stages = append([]string{}, e.stages...)
	res.StageDurationsSecs = e.durations
	res.TotalDurationSecs = time.Since(e.started).Seconds()
	e.audit("rehearsal_complete", "success", res.ApprovalID, decision,
		fmt.Sprintf(`{"rehearsal_id":%q,"policy_input_hash":%q}`, res.RehearsalID, finalPin.inputHash()))
	return res
}

// finalizeBlocked attaches stage observability to blocked results.
func (e *rehearsalEngine) finalizeBlocked(res *RehearsalResult) *RehearsalResult {
	if len(res.Stages) == 0 {
		res.Stages = append([]string{}, e.stages...)
	}
	if len(res.StageDurationsSecs) == 0 {
		res.StageDurationsSecs = e.durations
	}
	res.TotalDurationSecs = time.Since(e.started).Seconds()
	return res
}

// postRehearse implements POST /v1/migrations/{id}/rehearse with the strict
// semantic idempotency contract (same key + same body -> replay; same key +
// different body -> 409).
func postRehearse(w http.ResponseWriter, r *http.Request, workloadID, migrationID string) {
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
	var req RehearsalRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "invalid JSON", 400)
		return
	}
	req = req.withDefaults()
	if !canonicalStages[req.TargetWeight] {
		writeErr(w, rid, "VALIDATION_FAILED", "target_weight must be a canonical stage", 400)
		return
	}
	if req.Probes < 1 || req.Probes > 10 {
		writeErr(w, rid, "VALIDATION_FAILED", "probes must be 1..10", 400)
		return
	}
	if req.TimeoutSecs < 5 || req.TimeoutSecs > 300 {
		writeErr(w, rid, "VALIDATION_FAILED", "timeout_secs must be 5..300", 400)
		return
	}
	mu := idemLock("rehearse:" + key)
	mu.Lock()
	defer mu.Unlock()
	if resp, status, storedHash, found := store.CheckIdem("rehearse:" + key); found {
		if storedHash == "" || storedHash == hash {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(resp)
			return
		}
		idempotencyConflict(w, actor, "rehearse_migration", key, rid, AuditEntry{WorkloadID: workloadID, RunID: migrationID})
		return
	}
	eng := &rehearsalEngine{
		rid: rid, key: "rehearse:" + key, actor: actor,
		migrationID: migrationID, workloadID: workloadID,
		req: req, started: time.Now(), durations: map[string]float64{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(req.TimeoutSecs)*time.Second)
	defer cancel()
	res := eng.run(ctx)
	res = eng.finalizeBlocked(res)
	body, _ := json.Marshal(res)
	result := "success"
	if res.Status == RehearsalBlocked {
		result = "denied"
	}
	_ = store.RecordAudit(AuditEntry{
		RunID: migrationID, WorkloadID: workloadID, ActorType: actor.Type, ActorID: actor.ID,
		Action: "rehearse_migration", RequestID: rid, IdempotencyKey: key,
		PolicyBundleVersion: policyBundleVersion(), PolicyDecision: res.PolicyDecision,
		ApprovalID: res.ApprovalID, Result: result,
		Metadata: fmt.Sprintf(`{"rehearsal_id":%q,"status":%q}`, res.RehearsalID, res.Status),
	})
	store.SaveIdem("rehearse:"+key, hash, body, 200)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(body)
}

// rehearsalStagesJSON renders stage names deterministically for tests.
func rehearsalStagesJSON(stages []string) string {
	b, _ := json.Marshal(stages)
	return string(b)
}
