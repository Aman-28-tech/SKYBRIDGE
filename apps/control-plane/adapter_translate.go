// Shared adapter translation: plan components -> provider resource intents.
// Verdicts are consumed, never reinterpreted: blocked requirements fail the
// translation loudly instead of being dropped or downgraded.
package main

import (
	"fmt"
	"sort"
)

// providerKinds maps plan component keys to provider resource kinds.
func providerKinds(provider string) (map[string]string, error) {
	switch provider {
	case "azure":
		return map[string]string{
			"compute": "aks-cluster", "database": "postgresql-flexible",
			"cache": "redis-cache", "object-storage": "storage-blob",
			"queue": "servicebus-namespace", "identity": "workload-identity",
			"network": "virtual-network", "routing": "frontdoor-profile",
			"observability": "monitor-workspace", "security": "security-baseline",
		}, nil
	case "aws":
		return map[string]string{
			"compute": "eks-cluster", "database": "rds-postgres",
			"cache": "elasticache-redis", "object-storage": "s3-bucket",
			"queue": "sqs-queue", "identity": "iam-role",
			"network": "vpc-network", "routing": "alb-listener",
			"observability": "cloudwatch-config", "security": "security-baseline",
		}, nil
	default:
		return nil, adapterErr(ErrUnsupported, "unknown provider: "+provider, false)
	}
}

// logicalID is the stable cross-run identity of an intent.
func logicalID(provider, componentKey, kind string) string {
	return provider + ":" + componentKey + ":" + kind
}

// translatePlan performs the deterministic translation shared by providers.
func translatePlan(provider string, req ProvisioningRequest) (ProvisioningPlan, error) {
	if err := requireAuthorized(req); err != nil {
		return ProvisioningPlan{}, err
	}
	kinds, err := providerKinds(provider)
	if err != nil {
		return ProvisioningPlan{}, err
	}
	plan := req.Plan
	out := ProvisioningPlan{Provider: provider, PlanID: plan.ID, OperationKey: req.OperationKey}
	known := map[string]bool{}
	for _, c := range plan.Components {
		known[c.Key] = true
	}
	for _, c := range plan.Components {
		if c.Key == "data-movement" {
			// Derived metadata, not provisionable infrastructure: stated
			// explicitly so it is never silently discarded.
			out.Warnings = append(out.Warnings,
				"data-movement is derived execution metadata; no direct resources emitted")
			continue
		}
		kind, ok := kinds[c.Key]
		if !ok {
			return ProvisioningPlan{}, adapterErr(ErrUnsupported,
				fmt.Sprintf("no %s mapping for component %q", provider, c.Key), false)
		}
		if c.ProvisioningMode == ModeBlocked {
			return ProvisioningPlan{}, adapterErr(ErrUnsupported,
				fmt.Sprintf("component %q is blocked and has no executable action", c.Key), false)
		}
		if c.ProvisioningMode != ModeAuto && c.ProvisioningMode != ModeGated && c.ProvisioningMode != ModeDescribeOnly {
			return ProvisioningPlan{}, adapterErr(ErrInvalidConfig,
				fmt.Sprintf("component %q has unknown mode %q", c.Key, c.ProvisioningMode), false)
		}
		desired := map[string]any{}
		for k, v := range c.DesiredConfiguration {
			desired[k] = v
		}
		deps := []string{}
		for _, d := range c.Dependencies {
			dk, ok := kinds[d]
			if !ok {
				return ProvisioningPlan{}, adapterErr(ErrInvalidConfig,
					fmt.Sprintf("component %q depends on unmapped %q", c.Key, d), false)
			}
			deps = append(deps, logicalID(provider, d, dk))
		}
		sort.Strings(deps)
		out.Resources = append(out.Resources, ResourceIntent{
			LogicalID: logicalID(provider, c.Key, kind), Kind: kind,
			Provider: provider, Mode: c.ProvisioningMode, Desired: desired,
			Dependencies: deps, Evidence: append([]EvidenceRef{}, c.Evidence...),
		})
	}
	// RPO/RTO preserved for later CDC work (from data-movement config).
	for _, c := range plan.Components {
		if c.Key == "data-movement" {
			if v, ok := num(c.DesiredConfiguration["rpo_seconds"]); ok {
				out.RPOSeconds = int(v)
			}
			if v, ok := num(c.DesiredConfiguration["rto_seconds"]); ok {
				out.RTOSeconds = int(v)
			}
		}
	}
	out.Fingerprint = planFingerprint(out)
	return out, nil
}

// verifyPlan asserts plan self-consistency: every executable intent is
// configured, every dependency resolves, blocked modes never emit intents.
func verifyPlan(p ProvisioningPlan) (VerificationResult, error) {
	res := VerificationResult{Provider: p.Provider}
	known := map[string]bool{}
	for _, r := range p.Resources {
		known[r.LogicalID] = true
	}
	for _, r := range p.Resources {
		if r.Mode == ModeBlocked {
			return res, adapterErr(ErrVerifyFailed,
				"blocked intent entered provisioning plan: "+r.LogicalID, false)
		}
		if len(r.Desired) == 0 {
			return res, adapterErr(ErrVerifyFailed,
				"executable intent without configuration: "+r.LogicalID, false)
		}
		for _, d := range r.Dependencies {
			if !known[d] {
				return res, adapterErr(ErrVerifyFailed,
					fmt.Sprintf("unresolvable dependency %s of %s", d, r.LogicalID), false)
			}
		}
		res.Checks = append(res.Checks, "ok:"+r.LogicalID)
	}
	sort.Strings(res.Checks)
	res.Verified = true
	return res, nil
}
