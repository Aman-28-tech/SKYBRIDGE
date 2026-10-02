// AWS safe adapter: plan -> AWS intents (source-side reference mapping).
// The v1 migration direction is AWS -> Azure, so AWS intents describe the
// source footprint for describe/verify-side use. Same safety contract as
// Azure: pure translation, Apply unconditionally refuses.
package main

import "context"

type awsAdapter struct{}

func (awsAdapter) Provider() string { return "aws" }

func (awsAdapter) ValidateTarget(ctx context.Context, req ProvisioningRequest) error {
	if err := requireAuthorized(req); err != nil {
		return err
	}
	if req.Provider != "aws" {
		return adapterErr(ErrInvalidConfig, "request targets "+req.Provider+", adapter is aws", false)
	}
	return nil
}

func (a awsAdapter) PlanInfrastructure(ctx context.Context, req ProvisioningRequest) (ProvisioningPlan, error) {
	if err := a.ValidateTarget(ctx, req); err != nil {
		return ProvisioningPlan{}, err
	}
	return translatePlan("aws", req)
}

func (awsAdapter) ApplyInfrastructure(ctx context.Context, req ProvisioningRequest) error {
	return adapterErr(ErrApplyDisabled, "terraform apply is disabled in this slice; plan/validate only", false)
}

func (a awsAdapter) VerifyInfrastructure(ctx context.Context, plan ProvisioningPlan) (VerificationResult, error) {
	if plan.Provider != "aws" {
		return VerificationResult{}, adapterErr(ErrInvalidConfig, "plan is for "+plan.Provider, false)
	}
	return verifyPlan(plan)
}

func (a awsAdapter) DescribeInfrastructure(ctx context.Context, req ProvisioningRequest) (map[string]any, error) {
	if err := requireAuthorized(req); err != nil {
		return nil, err
	}
	plan, err := translatePlan("aws", req)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"provider": "aws", "operation_key": plan.OperationKey,
		"resources": len(plan.Resources), "fingerprint": plan.Fingerprint,
		// Demo seam: adapters read deterministic local fixtures, never cloud
		// APIs. The real-cloud path stays fail-closed (see DescribeLiveEnvironment).
		"source_of_truth": "LOCAL_FIXTURE",
		"mode": "mock-describe: translation only, no cloud reads",
	}, nil
}

// awsLiveIdentity reports whether real AWS API reads are configured. v1
// activates live reads only via explicit sandbox identity; otherwise every
// live path fails closed with ErrNoLiveCredentials (no network attempted).
func awsLiveIdentity() (string, error) {
	if role := getenv("SKYBRIDGE_AWS_ROLE_ARN", ""); role != "" {
		return role, nil
	}
	if profile := getenv("AWS_PROFILE", ""); profile != "" {
		return "profile:" + profile, nil
	}
	if getenv("AWS_ROLE_ARN", "") != "" {
		return getenv("AWS_ROLE_ARN", ""), nil
	}
	return "", adapterErr(ErrAuth, "no AWS sandbox identity configured (SKYBRIDGE_AWS_ROLE_ARN/AWS_PROFILE/AWS_ROLE_ARN); live reads disabled", false)
}

// DescribeLiveEnvironment is the future live-read entry point. It resolves
// identity and then refuses: real API calls land only with an approved
// sandbox + explicit budget (Phase 3 gate). Never falls back to mock data.
func (a awsAdapter) DescribeLiveEnvironment(ctx context.Context, req ProvisioningRequest) (map[string]any, error) {
	if err := requireAuthorized(req); err != nil {
		return nil, err
	}
	identity, err := awsLiveIdentity()
	if err != nil {
		return nil, err
	}
	return nil, adapterErr(ErrAuth,
		"live AWS reads not yet activated for identity "+identity+"; sandbox approval required", false)
}
