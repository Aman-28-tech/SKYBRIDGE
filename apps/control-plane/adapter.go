// Provider-neutral cloud adapter boundary.
//
// Target Migration Plan -> ProvisioningRequest -> CloudAdapter ->
// Terraform module boundary -> plan/validate -> verification.
//
// The SAME execution path serves every provider: only the adapter
// implementation differs. Adapters NEVER authorize (they reject contexts
// without authorization proof) and NEVER mutate in this slice: Apply is
// defined for future use and unconditionally refuses.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Adapter error codes (never collapsed into generic errors).
const (
	ErrUnsupported   = "unsupported"
	ErrInvalidConfig = "invalid_config"
	ErrTransient     = "transient"
	ErrAuth          = "auth"
	ErrValidation    = "validation"
	ErrPolicy        = "policy"
	ErrPlanFailed    = "plan_failed"
	ErrVerifyFailed  = "verification_failed"
	ErrApplyDisabled = "apply_disabled"
)

// AdapterError is a typed provider failure.
type AdapterError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *AdapterError) Error() string { return e.Code + ": " + e.Message }

func adapterErr(code, msg string, retryable bool) *AdapterError {
	return &AdapterError{Code: code, Message: msg, Retryable: retryable}
}

// ProvisioningRequest is the provider-neutral desired state for one plan.
// Authorized must be true (set only after the Policy Gate + approval pass
// inside the execution boundary) and PolicyInputHash must be non-empty;
// adapters reject anything else without touching anything.
type ProvisioningRequest struct {
	WorkloadID      string                 `json:"workload_id"`
	MigrationID     string                 `json:"migration_id"`
	WorkflowID      string                 `json:"workflow_id"`
	Provider        string                 `json:"provider"`
	Plan            TargetMigrationPlan    `json:"plan"`
	PolicyInputHash string                 `json:"policy_input_hash"`
	ApprovalID      string                 `json:"approval_id,omitempty"`
	Authorized      bool                   `json:"authorized"`
	OperationKey    string                 `json:"operation_key"`
}

// OperationKey deterministically identifies an infrastructure operation so
// retries can never create duplicates. It is NOT the HTTP idempotency key:
// it binds (plan, provider), surviving across API retries and restarts.
func OperationKey(planID, provider string) string {
	sum := sha256.Sum256([]byte(planID + "|" + provider))
	return "op-" + hex.EncodeToString(sum[:])[:16]
}

// NewProvisioningRequest binds a plan to a provider. It does not authorize;
// callers set Authorized only after the gate passes.
func NewProvisioningRequest(plan TargetMigrationPlan, provider, workloadID, migrationID, workflowID, inputHash, approvalID string) ProvisioningRequest {
	return ProvisioningRequest{
		WorkloadID: workloadID, MigrationID: migrationID, WorkflowID: workflowID,
		Provider: provider, Plan: plan, PolicyInputHash: inputHash, ApprovalID: approvalID,
		OperationKey: OperationKey(plan.ID, provider),
	}
}

// ResourceIntent is one desired provider resource. Desired carries the plan
// component configuration verbatim (never silently dropped); Mode mirrors
// the component: blocked requirements produce no intent (error instead).
type ResourceIntent struct {
	LogicalID    string         `json:"logical_id"`
	Kind         string         `json:"kind"`
	Provider     string         `json:"provider"`
	Mode         string         `json:"mode"`
	Desired      map[string]any `json:"desired"`
	Dependencies []string       `json:"dependencies"`
	Evidence     []EvidenceRef  `json:"evidence"`
}

// ProvisioningPlan is the deterministic provider translation of a target plan.
type ProvisioningPlan struct {
	Provider     string           `json:"provider"`
	PlanID       string           `json:"plan_id"`
	OperationKey string           `json:"operation_key"`
	Resources    []ResourceIntent `json:"resources"`
	Warnings     []string         `json:"warnings"`
	RPOSeconds   int              `json:"rpo_seconds"`
	RTOSeconds   int              `json:"rto_seconds"`
	Fingerprint  string           `json:"fingerprint"`
}

// VerificationResult is the mock verification outcome. In this slice it
// asserts plan self-consistency (every executable intent configured, every
// dependency resolvable); post-apply verification lands with real adapters.
type VerificationResult struct {
	Verified bool     `json:"verified"`
	Checks   []string `json:"checks"`
	Provider string   `json:"provider"`
}

// Fingerprint deterministically identifies a provisioning plan's content.
func planFingerprint(p ProvisioningPlan) string {
	b, _ := json.Marshal(struct {
		Provider  string           `json:"provider"`
		Resources []ResourceIntent `json:"resources"`
		RPO       int              `json:"rpo_seconds"`
		RTO       int              `json:"rto_seconds"`
	}{p.Provider, p.Resources, p.RPOSeconds, p.RTOSeconds})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// CloudAdapter is the provider boundary. Validate/Plan/Verify are pure;
// Apply exists for future use and MUST refuse in this slice.
type CloudAdapter interface {
	Provider() string
	ValidateTarget(ctx context.Context, req ProvisioningRequest) error
	PlanInfrastructure(ctx context.Context, req ProvisioningRequest) (ProvisioningPlan, error)
	ApplyInfrastructure(ctx context.Context, req ProvisioningRequest) error
	VerifyInfrastructure(ctx context.Context, plan ProvisioningPlan) (VerificationResult, error)
	DescribeInfrastructure(ctx context.Context, req ProvisioningRequest) (map[string]any, error)
}

// Adapters is the provider registry. Unknown providers fail closed.
var Adapters = map[string]CloudAdapter{
	"azure": azureAdapter{},
	"aws":   awsAdapter{},
}

// SelectAdapter resolves the provider or fails closed on unknown names.
func SelectAdapter(provider string) (CloudAdapter, error) {
	a, ok := Adapters[strings.ToLower(provider)]
	if !ok {
		return nil, adapterErr(ErrUnsupported, "unknown provider: "+provider, false)
	}
	return a, nil
}

// requireAuthorized enforces the authorization boundary inside every adapter
// entry point: no authorized context, no work.
func requireAuthorized(req ProvisioningRequest) error {
	if !req.Authorized {
		return adapterErr(ErrAuth, "provisioning request lacks authorization proof", false)
	}
	if req.PolicyInputHash == "" {
		return adapterErr(ErrAuth, "provisioning request lacks policy input hash", false)
	}
	if req.Plan.ID == "" {
		return adapterErr(ErrInvalidConfig, "provisioning request lacks target plan", false)
	}
	return nil
}
