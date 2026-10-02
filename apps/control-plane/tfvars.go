// Controlled provisioning readiness: Target Migration Plan -> Terraform
// module inputs, with a protected Apply boundary that stays disabled.
//
// Data flow (this slice): plan -> BuildTerraformVars -> fmt/validate/plan.
// Apply is defined but unconditionally refused: RequestApply always fails,
// and CheckApplyPreconditions gates the future path on fresh policy,
// approval, evidence, environment, and actor.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// ApplyEnabled is the hard switch for real mutation. False for this slice:
// no code path can reach Terraform apply while this is false.
const ApplyEnabled = false

// envRef marks a value supplied by the protected environment at apply time
// (resource group, subscription IDs, hostnames, secrets refs) — never
// hardcoded, never committed as a real value.
func envRef(name string) map[string]any {
	return map[string]any{"source": "env", "ref": name}
}

// TerraformVars is the deterministic module-input document for one provider.
type TerraformVars struct {
	Provider string                    `json:"provider"`
	PlanID   string                    `json:"plan_id"`
	Modules  map[string]map[string]any `json:"modules"`
	Enables  map[string]bool           `json:"enables"`
	Warnings []string                  `json:"warnings"`
}

// TerraformPlanDoc bundles vars with identity for inspection and audit.
type TerraformPlanDoc struct {
	Provider    string         `json:"provider"`
	PlanID      string         `json:"plan_id"`
	OperationKey string        `json:"operation_key"`
	Vars        TerraformVars  `json:"vars"`
	Warnings    []string       `json:"warnings"`
	Fingerprint string         `json:"fingerprint"`
}

func varsFingerprint(v TerraformVars) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func compConfig(plan TargetMigrationPlan, key string) map[string]any {
	for _, c := range plan.Components {
		if c.Key == key {
			if c.DesiredConfiguration == nil {
				return map[string]any{}
			}
			return c.DesiredConfiguration
		}
	}
	return nil
}

// BuildTerraformVars maps plan components to module inputs. Every material
// plan value is carried; stack-level values unknown to the plan become
// explicit env references (never invented, never hardcoded).
func BuildTerraformVars(plan TargetMigrationPlan, provider, workloadName string) (TerraformVars, error) {
	if provider != "azure" && provider != "aws" {
		return TerraformVars{}, adapterErr(ErrUnsupported, "unknown provider: "+provider, false)
	}
	get := func(key string) map[string]any {
		if m := compConfig(plan, key); m != nil {
			return m
		}
		return map[string]any{}
	}
	num := func(m map[string]any, k string, def int) int {
		if f, ok := num(m[k]); ok {
			return int(f)
		}
		return def
	}
	boolean := func(m map[string]any, k string, def bool) bool {
		if b, ok := m[k].(bool); ok {
			return b
		}
		return def
	}
	str := func(m map[string]any, k, def string) string {
		if s, ok := m[k].(string); ok {
			return s
		}
		return def
	}
	compute, database, cache := get("compute"), get("database"), get("cache")
	object, queue := get("object-storage"), get("queue")

	mods := map[string]map[string]any{}
	enables := map[string]bool{}
	mods["network"] = map[string]any{
		"name": workloadName, "resource_group_name": envRef("resource_group_name"),
		"location": envRef("location"),
	}
	enables["network"] = true
	mods["compute"] = map[string]any{
		"name": workloadName, "node_count": num(compute, "replicas", 1),
		"cpu_millicores": num(compute, "cpu_millicores", 0),
		"memory_mib":     num(compute, "memory_mib", 0),
		"zones":          zonesFromMin(compute),
		"subnet_id":      envRef("workload_subnet_id"),
	}
	enables["compute"] = true
	mods["database"] = map[string]any{
		"name": workloadName, "postgres_version": str(database, "major_version", ""),
		"storage_gb": num(database, "storage_gib", 0),
		"extensions": strList(database, "extensions"),
		"private_networking":  boolean(database, "private_networking", false),
		"publicly_accessible": boolean(database, "publicly_accessible", true),
		"delegated_subnet_id": envRef("data_subnet_id"),
		"private_dns_zone_id": envRef("private_dns_zone_id"),
		"admin_login":         envRef("db_admin_login"),
	}
	enables["database"] = true
	mods["cache"] = map[string]any{
		"name": workloadName, "authoritative": boolean(cache, "authoritative", true),
		"ttl_seconds": num(cache, "ttl_seconds", 0),
	}
	enables["cache"] = true
	mods["object"] = map[string]any{
		"name": workloadName, "required": boolean(object, "required", false),
		"versioning_required": boolean(object, "versioning_required", false),
		"identity_model":      str(object, "identity_model", ""),
		"container_name":      "cloudshop",
	}
	enables["object"] = boolean(object, "required", false)
	mods["queue"] = map[string]any{
		"name": workloadName, "delivery": str(queue, "delivery", ""),
		"duplicate_safe_consumer": boolean(queue, "duplicate_safe_consumer", false),
	}
	enables["queue"] = true
	mods["identity"] = map[string]any{
		"name": workloadName, "oidc_issuer_url": envRef("aks_oidc_issuer_url"),
		"subject": envRef("workload_service_account_subject"),
	}
	enables["identity"] = true
	mods["routing"] = map[string]any{
		"name": workloadName, "aws_origin_host": envRef("aws_origin_host"),
		"azure_origin_host": envRef("azure_origin_host"),
		"aws_weight": 100, "azure_weight": 0,
	}
	enables["routing"] = true
	mods["observability"] = map[string]any{
		"name": workloadName, "retention_days": 30,
	}
	enables["observability"] = true
	mods["security"] = map[string]any{
		"name": workloadName, "tenant_id": envRef("tenant_id"),
	}
	enables["security"] = true
	return TerraformVars{Provider: provider, PlanID: plan.ID, Modules: mods, Enables: enables}, nil
}

func zonesFromMin(compute map[string]any) []string {
	all := []string{"1", "2", "3"}
	mz := 0
	if f, ok := num(compute["min_zones"]); ok {
		mz = int(f)
	}
	if mz > 0 && mz < len(all) {
		return all[:mz]
	}
	return all
}

func strList(m map[string]any, k string) []string {
	var out []string
	if raw, ok := m[k].([]any); ok {
		for _, e := range raw {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	if raw, ok := m[k].([]string); ok {
		out = append(out, raw...)
	}
	if out == nil {
		return []string{}
	}
	sort.Strings(out)
	return out
}

// GenerateTerraformPlan builds the inspectable plan document. Blocked or
// unknown overall status refuses (no executable inputs); conditional emits
// with explicit warnings for the later approval gate.
func GenerateTerraformPlan(plan TargetMigrationPlan, provider, workloadName string) (TerraformPlanDoc, error) {
	switch plan.OverallStatus {
	case "block", "unknown":
		return TerraformPlanDoc{}, adapterErr(ErrUnsupported,
			"overall status "+plan.OverallStatus+": no executable provisioning inputs", false)
	}
	vars, err := BuildTerraformVars(plan, provider, workloadName)
	if err != nil {
		return TerraformPlanDoc{}, err
	}
	for _, c := range plan.Components {
		switch c.ProvisioningMode {
		case ModeGated, ModeDescribeOnly:
			vars.Warnings = append(vars.Warnings,
				"component "+c.Key+" is "+c.ProvisioningMode+": inputs emitted for review, gated before apply")
		case ModeBlocked:
			return TerraformPlanDoc{}, adapterErr(ErrUnsupported,
				"component "+c.Key+" is blocked: no executable inputs", false)
		}
	}
	doc := TerraformPlanDoc{Provider: provider, PlanID: plan.ID,
		OperationKey: OperationKey(plan.ID, provider), Vars: vars, Warnings: vars.Warnings}
	doc.Fingerprint = varsFingerprint(vars)
	return doc, nil
}

// StateKey isolates Terraform state per environment, workload, and scope.
func StateKey(environment, workloadID, scope string) string {
	return "skybridge/" + environment + "/" + workloadID + "/" + scope + ".tfstate"
}

// ApplyPreconditions is the future apply gate input.
type ApplyPreconditions struct {
	Decision            string
	Approval            *Approval
	PolicyInputHash     string
	PolicyBundleVersion string
	PlanFresh           bool
	CompatFresh         bool
	DriftFresh          bool
	Environment         string
	Actor               string
}

// CheckApplyPreconditions validates everything Apply will ever require.
// It never authorizes by itself: RequestApply additionally requires the
// ApplyEnabled hard switch, which is false in this slice.
func CheckApplyPreconditions(p ApplyPreconditions) error {
	if p.Decision == PolicyDeny {
		return adapterErr(ErrPolicy, "policy denies; apply refused", false)
	}
	if p.Decision != PolicyAllow {
		if p.Approval == nil {
			return adapterErr(ErrPolicy, "approval required but none supplied", false)
		}
		a := p.Approval
		if a.Decision != ApprovalApproved {
			return adapterErr(ErrPolicy, "approval is not approved: "+a.Decision, false)
		}
		if approvalExpired(*a) {
			return adapterErr(ErrPolicy, "approval expired", false)
		}
		if a.PolicyInputHash != p.PolicyInputHash {
			return adapterErr(ErrPolicy, "approval bound to different policy inputs", false)
		}
	}
	if !p.PlanFresh || !p.CompatFresh || !p.DriftFresh {
		return adapterErr(ErrPolicy, "stale evidence; re-evaluate before apply", false)
	}
	switch p.Environment {
	case "staging", "production-like", "experiment":
	default:
		return adapterErr(ErrPolicy, "apply only in protected environments", false)
	}
	if p.Actor == "" {
		return adapterErr(ErrAuth, "authorized actor required", false)
	}
	return nil
}

// RequestApply is the future mutation entry point. Disabled in this slice:
// it always refuses, even when preconditions pass, so no code path can
// reach Terraform apply until the switch is deliberately flipped with
// review in a later slice.
func RequestApply(p ApplyPreconditions) error {
	if err := CheckApplyPreconditions(p); err != nil {
		return err
	}
	if !ApplyEnabled {
		return adapterErr(ErrApplyDisabled, "terraform apply is disabled in this slice", false)
	}
	return adapterErr(ErrApplyDisabled, "unreachable", false)
}
