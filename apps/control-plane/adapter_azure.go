// Azure safe adapter: plan -> Azure intents. Pure translation; the only
// mutation surface (Apply) unconditionally refuses in this slice.
package main

import "context"

type azureAdapter struct{}

func (azureAdapter) Provider() string { return "azure" }

func (azureAdapter) ValidateTarget(ctx context.Context, req ProvisioningRequest) error {
	if err := requireAuthorized(req); err != nil {
		return err
	}
	if req.Provider != "azure" {
		return adapterErr(ErrInvalidConfig, "request targets "+req.Provider+", adapter is azure", false)
	}
	if req.Plan.TargetProvider != "azure" {
		return adapterErr(ErrInvalidConfig, "plan targets "+req.Plan.TargetProvider, false)
	}
	return nil
}

func (a azureAdapter) PlanInfrastructure(ctx context.Context, req ProvisioningRequest) (ProvisioningPlan, error) {
	if err := a.ValidateTarget(ctx, req); err != nil {
		return ProvisioningPlan{}, err
	}
	return translatePlan("azure", req)
}

func (azureAdapter) ApplyInfrastructure(ctx context.Context, req ProvisioningRequest) error {
	return adapterErr(ErrApplyDisabled, "terraform apply is disabled in this slice; plan/validate only", false)
}

func (a azureAdapter) VerifyInfrastructure(ctx context.Context, plan ProvisioningPlan) (VerificationResult, error) {
	if plan.Provider != "azure" {
		return VerificationResult{}, adapterErr(ErrInvalidConfig, "plan is for "+plan.Provider, false)
	}
	return verifyPlan(plan)
}

func (a azureAdapter) DescribeInfrastructure(ctx context.Context, req ProvisioningRequest) (map[string]any, error) {
	if err := requireAuthorized(req); err != nil {
		return nil, err
	 }
	plan, err := translatePlan("azure", req)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"provider": "azure", "operation_key": plan.OperationKey,
		"resources": len(plan.Resources), "fingerprint": plan.Fingerprint,
		// Demo seam: adapters read deterministic local fixtures, never cloud
		// APIs. The real-cloud path stays fail-closed (see DescribeLiveEnvironment).
		"source_of_truth": "LOCAL_FIXTURE",
		"mode": "mock-describe: translation only, no cloud reads",
	}, nil
}
