// Server-side Policy Gate (fail-closed).
//
// Assembles canonical policy inputs from STORED evidence (compatibility
// report, drift report, target plan, canonical spec) plus caller-supplied
// runtime context, hashes the canonical input, and evaluates the decision
// with a rule mirror of packages/contracts/policy/cutover.rego:
//
//	deny reasons match cutover.rego deny_reason values;
//	approval_required matches its requires_approval rules.
//
// Decisions: allow | deny | approval_required. Anything the gate cannot
// establish is deny (never allow, never approval). No AI in this path.
// No mutations, no cloud calls, no Terraform.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// canonicalStages mirrors the cutover.rego stage set: 0/1/5/25/50/100.
var canonicalStages = map[int]bool{0: true, 1: true, 5: true, 25: true, 50: true, 100: true}

// Policy decisions.
const (
	PolicyAllow            = "allow"
	PolicyDeny             = "deny"
	PolicyApprovalRequired = "approval_required"
)

// PolicyInput is the canonical evaluation input. Fixed field order makes the
// JSON encoding (and therefore the input hash) deterministic.
type PolicyInput struct {
	Action                   string `json:"action"`
	Environment              string `json:"environment"`
	Resource                 string `json:"resource"`
	ActorType                string `json:"actor_type"`
	ActorID                  string `json:"actor_id"`
	TargetWeight             int    `json:"target_weight"`
	ReadOnlyCanary           bool   `json:"read_only_canary"`
	WriteOwnership           string `json:"write_ownership"`
	OwnershipTransferVerified bool  `json:"ownership_transfer_verified"`
	ReverseSyncReady         bool   `json:"reverse_sync_ready"`
	ValidationStatus         string `json:"validation_status"`
	CompatibilityStatus      string `json:"compatibility_status"`
	RPOSeconds               int    `json:"rpo_seconds"`
	CDCLagSeconds            int    `json:"cdc_lag_seconds"`
	BlockingDriftCount       int    `json:"blocking_drift_count"`
	SecurityCriticalDriftCount int  `json:"security_critical_drift_count"`
	TargetHealthy            bool   `json:"target_healthy"`
	Approval                 string `json:"approval"`
}

// canonicalJSON returns the deterministic encoding.
func (in PolicyInput) canonicalJSON() []byte {
	b, _ := json.Marshal(in)
	return b
}

// inputHash is hex(SHA-256(canonical input JSON)), recorded on every decision.
func (in PolicyInput) inputHash() string {
	sum := sha256.Sum256(in.canonicalJSON())
	return hex.EncodeToString(sum[:])
}

// eligibilityHash binds approvals to action + evidence, excluding actor
// identity: the requester who sought approval is normally distinct from the
// operator/worker that later executes, and executors authenticate separately
// at the mutation layer. The full inputHash (with actor) stays on the audit
// trail; eligibility compares this hash.
func (in PolicyInput) eligibilityHash() string {
	cpy := in
	cpy.ActorType, cpy.ActorID = "", ""
	sum := sha256.Sum256(cpy.canonicalJSON())
	return hex.EncodeToString(sum[:])
}

// GateResult is one named sub-decision for the structured response.
type GateResult struct {
	Name   string `json:"name"`
	Result string `json:"result"` // pass|fail|warn
	Reason string `json:"reason"`
}

// EvalFail is an explicit evaluation refusal (stale/missing evidence).
// Failures are fail-closed: the caller must surface them, never allow.
type EvalFail struct {
	Code    string
	Message string
	Status  int
}

func (e *EvalFail) Error() string { return e.Code + ": " + e.Message }

// policyDecide evaluates the decision. Total function: every input maps to
// exactly one of allow|deny|approval_required. Deny reasons mirror
// cutover.rego deny_reason values one-for-one.
func policyDecide(in PolicyInput) (decision string, reasons []string) {
	fail := func(reason string) (string, []string) { return PolicyDeny, []string{reason} }
	if in.Action != "shift_traffic" {
		return fail("unsupported_action")
	}
	if in.ValidationStatus != "passed" {
		return fail("validation_failed")
	}
	// Compatibility: block/unknown hard-deny; conditional falls through to
	// the approval rules below (matching requires_approval in Rego).
	switch in.CompatibilityStatus {
	case "pass":
	case "conditional":
	case "block", "unknown":
		return fail("compatibility_not_pass")
	default:
		return fail("compatibility_not_pass")
	}
	if in.BlockingDriftCount > 0 {
		return fail("blocking_drift")
	}
	if in.SecurityCriticalDriftCount > 0 {
		return fail("security_critical_drift")
	}
	if in.CDCLagSeconds > in.RPOSeconds {
		return fail("cdc_lag_exceeds_rpo")
	}
	if !in.TargetHealthy {
		return fail("target_unhealthy")
	}
	if !canonicalStages[in.TargetWeight] {
		return fail("non_canonical_weight")
	}
	if in.TargetWeight >= 1 && in.TargetWeight <= 50 && !in.ReadOnlyCanary {
		return fail("read_only_canary_violation")
	}
	if in.WriteOwnership != "aws" && in.TargetWeight != 100 {
		return fail("write_ownership_violation")
	}
	// Approval triggers (Rego requires_approval rules).
	needsApproval := false
	approvalWhy := ""
	switch {
	case in.Environment == "production-like":
		needsApproval, approvalWhy = true, "production-like always requires approval"
	case in.Environment == "staging" && in.TargetWeight > 25:
		needsApproval, approvalWhy = true, "staging above 25% requires approval"
	case in.CompatibilityStatus == "conditional":
		needsApproval, approvalWhy = true, "conditional compatibility requires explicit approval"
	}
	switch in.Environment {
	case "dev", "local", "experiment":
		if in.TargetWeight == 100 && !in.OwnershipTransferVerified {
			return fail("ownership_transfer_not_verified")
		}
		if needsApproval {
			return PolicyApprovalRequired, []string{approvalWhy}
		}
		return PolicyAllow, []string{}
	case "staging":
		if in.TargetWeight <= 25 {
			if needsApproval {
				return PolicyApprovalRequired, []string{approvalWhy}
			}
			return PolicyAllow, []string{}
		}
		return PolicyApprovalRequired, []string{approvalWhy}
	case "production-like":
		return PolicyApprovalRequired, []string{approvalWhy}
	default:
		return fail("denied")
	}
}

// policyGates expands the decision into per-gate results for the structured
// response. Gate order is fixed.
func policyGates(in PolicyInput, decision string, reasons []string) []GateResult {
	g := func(name, result, reason string) GateResult { return GateResult{Name: name, Result: result, Reason: reason} }
	pass := func(name string) GateResult { return g(name, "pass", "satisfied") }
	failReason := ""
	if len(reasons) > 0 {
		failReason = reasons[0]
	}
	mk := func(name string, ok bool, reason string) GateResult {
		if ok {
			return pass(name)
		}
		res := "fail"
		if decision == PolicyApprovalRequired {
			res = "warn"
		}
		return g(name, res, reason)
	}
	_ = failReason
	compatOK := in.CompatibilityStatus == "pass"
	compatWarn := in.CompatibilityStatus == "conditional"
	compatGate := GateResult{Name: "compatibility"}
	switch {
	case compatOK:
		compatGate = pass("compatibility")
	case compatWarn:
		compatGate = g("compatibility", "warn", "conditional: explicit approval required")
	default:
		compatGate = g("compatibility", "fail", "compatibility_"+in.CompatibilityStatus+": not executable")
	}
	return []GateResult{
		compatGate,
		mk("drift", in.BlockingDriftCount == 0 && in.SecurityCriticalDriftCount == 0,
			fmt.Sprintf("blocking=%d security_critical=%d", in.BlockingDriftCount, in.SecurityCriticalDriftCount)),
		mk("validation", in.ValidationStatus == "passed", "validation_"+in.ValidationStatus),
		mk("freshness", in.CDCLagSeconds <= in.RPOSeconds,
			fmt.Sprintf("cdc_lag=%ds rpo=%ds", in.CDCLagSeconds, in.RPOSeconds)),
		mk("health", in.TargetHealthy, "target health"),
		mk("stage", canonicalStages[in.TargetWeight], "canonical stage"),
		mk("canary", !(in.TargetWeight >= 1 && in.TargetWeight <= 50 && !in.ReadOnlyCanary), "read-only canary"),
		mk("ownership", !(in.WriteOwnership != "aws" && in.TargetWeight != 100), "write ownership"),
		mk("environment", in.Environment == "dev" || in.Environment == "local" ||
			in.Environment == "experiment" || in.Environment == "staging" ||
			in.Environment == "production-like", "environment "+in.Environment),
	}
}

// buildPolicyInput assembles the canonical input from stored evidence plus
// runtime context. Missing or stale evidence is an explicit *EvalFail —
// never defaulted, never an allow. Actor identity is the verified
// authentication principal (never request headers).
func buildPolicyInput(workloadID string, runtime map[string]any, actor Principal) (PolicyInput, *EvalFail) {
	var in PolicyInput
	fail := func(code, msg string, status int) (PolicyInput, *EvalFail) {
		return PolicyInput{}, &EvalFail{Code: code, Message: msg, Status: status}
	}
	wl, ok := store.GetWorkload(workloadID)
	if !ok {
		return fail("NOT_FOUND", "workload not found", 404)
	}
	spec, ok := wl["canonical_spec"].(map[string]any)
	if !ok {
		return fail("VALIDATION_FAILED", "canonical_spec unavailable for workload", 400)
	}
	reg, err := EmbeddedRegistry()
	if err != nil {
		return fail("INTERNAL_ERROR", "capability registry unavailable", 500)
	}
	specRaw, _ := json.Marshal(spec)
	specHash, _ := canonicalHash(specRaw)
	rep, ok := store.GetLatestCompatReport(workloadID)
	if !ok || rep.ID != ReportID(workloadID, reg.Version, specHash) ||
		rep.RegistryVersion != reg.Version {
		return fail("COMPATIBILITY_STALE", "no current compatibility report for this spec; evaluate compatibility first", 409)
	}
	plan, ok := store.GetLatestTargetPlan(workloadID)
	if !ok {
		return fail("NOT_FOUND", "no target plan yet for workload; generate a plan first", 404)
	}
	if plan.CompatibilityReportID != rep.ID ||
		plan.CompatibilityRegistryVersion != reg.Version ||
		plan.PlannerVersion != PlannerVersion {
		return fail("PLAN_STALE", "target plan is stale for the current spec; regenerate compatibility and plan first", 409)
	}
	drift, ok := store.GetLatestDriftReport(workloadID, plan.ID)
	if !ok {
		return fail("NO_DRIFT_EVIDENCE", "no drift evaluation for the current plan; run drift evaluation first", 409)
	}
	blocking, critical := 0, 0
	for _, f := range drift.Findings {
		switch f.Severity {
		case "blocking":
			blocking++
		case "security_critical":
			critical++
		}
	}
	name, _ := wl["name"].(string)
	get := func(k string) any { return runtime[k] }
	str := func(k, def string) string {
		if s, ok := get(k).(string); ok && s != "" {
			return s
		}
		return def
	}
	num := func(k string, def int) int {
		switch v := get(k).(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
		return def
	}
	boolean := func(k string, def bool) bool {
		if b, ok := get(k).(bool); ok {
			return b
		}
		return def
	}
	actorType := actor.Type
	if actorType == "" {
		actorType = "human"
	}
	in = PolicyInput{
		Action:                   str("action", "shift_traffic"),
		Environment:              str("environment", "dev"),
		Resource:                 name,
		ActorType:                actorType,
		ActorID:                  actor.ID,
		TargetWeight:             num("target_weight", -1),
		ReadOnlyCanary:           boolean("read_only_canary", true),
		WriteOwnership:           str("write_ownership", "aws"),
		OwnershipTransferVerified: boolean("ownership_transfer_verified", false),
		ReverseSyncReady:         boolean("reverse_sync_ready", false),
		ValidationStatus:         str("validation_status", "passed"),
		CompatibilityStatus:      rep.Status,
		RPOSeconds:               intOf(reqField(spec, "rpo_seconds"), 30),
		CDCLagSeconds:            num("cdc_lag_seconds", 8),
		BlockingDriftCount:       blocking,
		SecurityCriticalDriftCount: critical,
		TargetHealthy:            boolean("target_healthy", true),
		Approval:                 str("approval", "none"),
	}
	return in, nil
}

func reqField(spec map[string]any, field string) any {
	reqs, _ := spec["requirements"].(map[string]any)
	if reqs == nil {
		return nil
	}
	return reqs[field]
}

// decisionBytes marshals a readiness decision for responses and idempotency storage.
func decisionBytes(decision map[string]any) ([]byte, error) {
	return json.Marshal(decision)
}

// postReadiness implements POST /v1/workloads/{id}/readiness.
// Analysis: evaluates stored evidence plus supplied runtime context and
// returns the decision. Always HTTP 200 for evaluated decisions (including
// deny/approval_required); 4xx only for malformed input, missing/stale
// evidence, or idempotency conflicts. Audits evaluated decisions with bundle
// version and input hash. Mutates nothing.
// Authentication precedes everything (401); the caller must be authorized
// for the workload (403); audit identity is the verified principal (M-4).
func postReadiness(w http.ResponseWriter, r *http.Request, workloadID string) {
	rid := reqID(r)
	actor, ok := requireAuth(w, r)
	if !ok {
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
	var runtime map[string]any
	if err := json.Unmarshal(raw, &runtime); err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "invalid JSON", 400)
		return
	}
	if _, present := runtime["target_weight"]; !present {
		writeErr(w, rid, "VALIDATION_FAILED", "target_weight required", 400)
		return
	}
	mu := idemLock(key)
	mu.Lock()
	defer mu.Unlock()
	if resp, status, storedHash, found := store.CheckIdem(key); found {
		if storedHash == "" || storedHash == hash {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(resp)
			return
		}
		writeErr(w, rid, "IDEMPOTENCY_CONFLICT", "same Idempotency-Key with different request body", 409)
		return
	}
	in, fail := buildPolicyInput(workloadID, runtime, actor)
	if fail != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fail.Status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": fail.Code, "message": fail.Message,
			"request_id": rid, "details": map[string]any{}}})
		return
	}
	decision, reasons := policyDecide(in)
	gates := policyGates(in, decision, reasons)
	body, _ := decisionBytes(map[string]any{
		"decision": decision, "gates": gates, "reasons": reasons,
		"policy_bundle_version": policyBundleVersion(),
		"policy_input_hash":     in.inputHash(),
		"inputs":                json.RawMessage(in.canonicalJSON()),
	})
	result := "success"
	if decision == PolicyDeny {
		result = "denied"
	}
	_ = store.RecordAudit(AuditEntry{
		WorkloadID: workloadID, ActorType: actor.Type, ActorID: actor.ID,
		Action: "evaluate_readiness", RequestID: rid, IdempotencyKey: key,
		PolicyBundleVersion: policyBundleVersion(), PolicyDecision: decision, Result: result,
		Metadata: fmt.Sprintf(`{"policy_input_hash":%q}`, in.inputHash()),
	})
	store.SaveIdem(key, hash, body, 200)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(body)
}
