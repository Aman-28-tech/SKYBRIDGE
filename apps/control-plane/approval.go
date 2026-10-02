// Approval workflow (explicit human authorization for policy-gated actions).
//
// Architecture preserved: authorization -> policy gate -> approval (this
// file) -> future workflow -> adapter -> verification. Approval NEVER
// executes anything: it only makes a gated action eligible, and only while
// its bound evidence stays current. AI can propose; only a human actor
// distinct from the requester can decide (actor_type human enforced).
//
// States use the domain enum exactly: pending|approved|rejected. Expiry is
// temporal (expires_at); there is no revoked state in v1 (rejection is the
// negative path).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Approval risk levels (domain enum).
const (
	RiskLow      = "low"
	RiskMedium   = "medium"
	RiskHigh     = "high"
	RiskCritical = "critical"
)

// Approval decisions (domain enum).
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
)

// approvalExpiry is the v1 approval lifetime.
const approvalExpiry = 24 * time.Hour

// parseApprovalTime parses timestamps from either store. MemStore keeps
// RFC3339; PostgreSQL text output uses "2006-01-02 15:04:05[.fraction]-07".
// A timestamp that cannot be parsed is treated as missing (fail-closed
// callers must handle ok=false as expired/unusable).
func parseApprovalTime(s string) (time.Time, bool) {
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07",
		"2006-01-02 15:04:05-07",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// approvalExpired reports whether the approval is past its lifetime.
// Unparseable expiry fails closed (treated as expired).
func approvalExpired(appr Approval) bool {
	exp, ok := parseApprovalTime(appr.ExpiresAt)
	return !ok || time.Now().After(exp)
}

// Approval binds a gated decision to its exact evidence context.
type Approval struct {
	ID                   string `json:"id"`
	WorkloadID           string `json:"workload_id"`
	MigrationID          string `json:"migration_id,omitempty"`
	Action               string `json:"action"`
	Risk                 string `json:"risk"`
	Decision             string `json:"decision"`
	RequestedBy          string `json:"requested_by"`
	RequestedByType      string `json:"requested_by_type"`
	DecidedBy            string `json:"decided_by,omitempty"`
	DecidedByType        string `json:"decided_by_type,omitempty"`
	PolicyBundleVersion  string `json:"policy_bundle_version"`
	PolicyInputHash      string `json:"policy_input_hash"`
	TargetProvider       string `json:"target_provider"`
	PlanID               string `json:"plan_id,omitempty"`
	CompatibilityReportID string `json:"compatibility_report_id,omitempty"`
	DriftReportID        string `json:"drift_report_id,omitempty"`
	TargetWeight         int    `json:"target_weight"`
	Environment          string `json:"environment"`
	ExpiresAt            string `json:"expires_at"`
	DecidedAt            string `json:"decided_at,omitempty"`
	CreatedAt            string `json:"created_at,omitempty"`
}

// riskFor maps environment to risk (deterministic, documented).
func riskFor(environment string) string {
	switch environment {
	case "production-like":
		return RiskCritical
	case "staging":
		return RiskHigh
	default:
		return RiskMedium
	}
}

// requesterDistinctness and human-only decision are enforced in
// decideApproval from the verified authentication principal; header-claimed
// actor identity is never trusted (H-1).

// postApproval implements POST /v1/workloads/{id}/approvals.
// Assembles current evidence exactly like the readiness gate; only an
// approval_required decision may be bound into a pending approval.
// Deny can never become approvable; allow needs no approval.
// Authentication precedes everything (401); the requester must be authorized
// for the workload (403); requested_by binds the verified principal (M-4).
func postApproval(w http.ResponseWriter, r *http.Request, workloadID string) {
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
	if decision == PolicyDeny {
		writeErr(w, rid, "POLICY_DENIED", "denied policy cannot be approved into execution: "+strings.Join(reasons, "; "), 409)
		return
	}
	if decision == PolicyAllow {
		writeErr(w, rid, "APPROVAL_NOT_REQUIRED", "policy allows; no approval needed", 409)
		return
	}
	// approval_required: bind the exact evidence context.
	plan, _ := store.GetLatestTargetPlan(workloadID)
	rep, _ := store.GetLatestCompatReport(workloadID)
	drift, _ := store.GetLatestDriftReport(workloadID, plan.ID)
	appr := Approval{
		ID: uuid(), WorkloadID: workloadID,
		Action: in.Action, Risk: riskFor(in.Environment), Decision: ApprovalPending,
		RequestedBy: actor.ID, RequestedByType: actor.Type,
		PolicyBundleVersion: policyBundleVersion(), PolicyInputHash: in.eligibilityHash(),
		TargetProvider: "azure", // v1 target; endpoints reject anything else
		PlanID: plan.ID, CompatibilityReportID: rep.ID, DriftReportID: drift.ID,
		TargetWeight: in.TargetWeight, Environment: in.Environment,
		ExpiresAt: time.Now().Add(approvalExpiry).UTC().Format(time.RFC3339),
	}
	if err := store.SaveApproval(appr); err != nil {
		writeErr(w, rid, "INTERNAL_ERROR", "approval persist failed", 500)
		return
	}
	_ = store.RecordAudit(AuditEntry{
		WorkloadID: workloadID, ActorType: actor.Type, ActorID: actor.ID,
		Action: "request_approval", RequestID: rid, IdempotencyKey: key,
		PolicyBundleVersion: policyBundleVersion(), PolicyDecision: PolicyApprovalRequired,
		Result: "success", Metadata: fmt.Sprintf(`{"policy_input_hash":%q,"approval_id":%q}`, in.eligibilityHash(), appr.ID),
	})
	resp, _ := json.Marshal(appr)
	store.SaveIdem(key, hash, resp, 201)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	_, _ = w.Write(resp)
}

// decideApproval implements POST /v1/workloads/{id}/approvals/{approvalId}/decision.
// Only a verified human principal distinct from the requester may decide
// (actor identity from authentication material, never headers), only while
// pending and unexpired, and only against still-current evidence. The decider
// must additionally be authorized for the workload (H-2).
func decideApproval(w http.ResponseWriter, r *http.Request, workloadID, approvalID string) {
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
	var in struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || (in.Decision != ApprovalApproved && in.Decision != ApprovalRejected) {
		writeErr(w, rid, "VALIDATION_FAILED", "decision must be approved or rejected", 400)
		return
	}
	mu := idemLock("approval-decision:" + approvalID)
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
	appr, ok := store.GetApproval(approvalID)
	if !ok || appr.WorkloadID != workloadID {
		writeErr(w, rid, "NOT_FOUND", "approval not found", 404)
		return
	}
	if appr.Decision != ApprovalPending {
		writeErr(w, rid, "APPROVAL_SETTLED", "approval already decided: "+appr.Decision, 409)
		return
	}
	if approvalExpired(appr) {
		writeErr(w, rid, "APPROVAL_EXPIRED", "approval expired; request a new approval", 409)
		return
	}
	decider, deciderType := actor.ID, actor.Type
	if deciderType != "human" {
		writeErr(w, rid, "APPROVAL_FORBIDDEN", "only human actors may decide approvals", 403)
		return
	}
	if decider == "" || decider == appr.RequestedBy {
		writeErr(w, rid, "APPROVAL_FORBIDDEN", "requester cannot decide their own approval", 403)
		return
	}
	// Revalidate evidence currency before recording any decision.
	if stale := approvalStaleness(workloadID, appr); stale != "" {
		writeErr(w, rid, "APPROVAL_STALE", stale+"; request a new approval", 409)
		return
	}
	updated := appr
	updated.Decision = in.Decision
	updated.DecidedBy = decider
	updated.DecidedByType = deciderType
	updated.DecidedAt = time.Now().UTC().Format(time.RFC3339)
	changed, err := store.DecideApproval(updated)
	if err != nil || !changed {
		writeErr(w, rid, "APPROVAL_SETTLED", "approval already decided", 409)
		return
	}
	_ = store.RecordAudit(AuditEntry{
		WorkloadID: workloadID, ActorType: deciderType, ActorID: decider,
		Action: "decide_approval", RequestID: rid, IdempotencyKey: key,
		PolicyBundleVersion: appr.PolicyBundleVersion, PolicyDecision: in.Decision,
		Result: "success", ApprovalID: approvalID,
		Metadata: fmt.Sprintf(`{"policy_input_hash":%q}`, appr.PolicyInputHash),
	})
	resp, _ := json.Marshal(updated)
	store.SaveIdem(key, hash, resp, 200)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(resp)
}

// getApprovalList implements GET /v1/workloads/{id}/approvals[?status=].
func getApprovalList(w http.ResponseWriter, r *http.Request, workloadID string) {
	rid := reqID(r)
	if _, ok := store.GetWorkload(workloadID); !ok {
		writeErr(w, rid, "NOT_FOUND", "workload not found", 404)
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "", ApprovalPending, ApprovalApproved, ApprovalRejected:
	default:
		writeErr(w, rid, "VALIDATION_FAILED", "invalid status filter", 400)
		return
	}
	items := store.GetApprovals(workloadID, status)
	if items == nil {
		items = []Approval{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
}

// approvalStaleness revalidates an approval's bound evidence. Empty means fresh.
func approvalStaleness(workloadID string, appr Approval) string {
	wl, ok := store.GetWorkload(workloadID)
	if !ok {
		return "workload not found"
	}
	spec, ok := wl["canonical_spec"].(map[string]any)
	if !ok {
		return "canonical_spec unavailable"
	}
	reg, err := EmbeddedRegistry()
	if err != nil {
		return "capability registry unavailable"
	}
	specRaw, _ := json.Marshal(spec)
	specHash, _ := canonicalHash(specRaw)
	rep, ok := store.GetLatestCompatReport(workloadID)
	if !ok || rep.ID != ReportID(workloadID, reg.Version, specHash) ||
		rep.RegistryVersion != reg.Version || appr.CompatibilityReportID != rep.ID {
		return "compatibility evidence changed"
	}
	plan, ok := store.GetLatestTargetPlan(workloadID)
	if !ok || plan.ID != appr.PlanID || plan.PlannerVersion != PlannerVersion ||
		plan.CompatibilityReportID != rep.ID {
		return "target plan changed"
	}
	drift, ok := store.GetLatestDriftReport(workloadID, plan.ID)
	if !ok || drift.ID != appr.DriftReportID {
		return "drift evidence changed"
	}
	if appr.PolicyBundleVersion != policyBundleVersion() {
		return "policy bundle changed"
	}
	return ""
}

// approvalEligibleForUse validates an approval_id supplied at execution time
// against a freshly evaluated policy input. Returns the approval or a fail
// code/message for the caller to surface.
func approvalEligibleForUse(workloadID, approvalID string, in PolicyInput) (*Approval, string, string) {
	if approvalID == "" {
		return nil, "APPROVAL_REQUIRED", "approval_required: explicit approval needed"
	}
	appr, ok := store.GetApproval(approvalID)
	if !ok || appr.WorkloadID != workloadID {
		return nil, "APPROVAL_INVALID", "approval not found"
	}
	if appr.Decision != ApprovalApproved {
		return nil, "APPROVAL_INVALID", "approval is not approved: " + appr.Decision
	}
	if approvalExpired(appr) {
		return nil, "APPROVAL_EXPIRED", "approval expired"
	}
	if appr.PolicyInputHash != in.eligibilityHash() {
		return nil, "APPROVAL_STALE", "approval bound to different policy inputs; re-evaluate and re-approve"
	}
	// Freshness of the world behind the hash (belt and suspenders: hashes
	// cover content, this covers liveness/version drift).
	if stale := approvalStaleness(workloadID, appr); stale != "" {
		return nil, "APPROVAL_STALE", stale
	}
	// Even with approval, a deny can never execute (rule 4, enforced at use).
	if dec, _ := policyDecide(in); dec == PolicyDeny {
		return nil, "CUTOVER_DENIED", "policy denies despite approval"
	}
	return &appr, "", ""
}
