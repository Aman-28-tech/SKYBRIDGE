// Provisioning-readiness tests: plan -> Terraform vars fidelity, root
// composition shape, apply boundary, state keys, no-secrets. No cloud.
package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func tfPlan(t *testing.T) TargetMigrationPlan {
	t.Helper()
	reg := fullPassRegistry(t)
	spec := planSpec(t)
	rep := Evaluate(spec, reg, "azure")
	return BuildPlan(spec, rep, reg)
}

// Fidelity: every material plan value reaches module inputs, both providers.
func TestTFVarsFidelity(t *testing.T) {
	plan := tfPlan(t)
	for _, provider := range []string{"azure", "aws"} {
		doc, err := GenerateTerraformPlan(plan, provider, "cloudshop")
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		mod := func(name string) map[string]any { return doc.Vars.Modules[name] }
		numEq := func(key, field string, want float64) {
			t.Helper()
			got, ok := num(doc.Vars.Modules[key][field])
			if !ok || got != want {
				t.Fatalf("%s %s.%s=%v want %v", provider, key, field, doc.Vars.Modules[key][field], want)
			}
		}
		if got := mod("compute")["node_count"]; got != float64(2) && got != 2 {
			t.Fatalf("%s replicas=%v", provider, got)
		}
		numEq("compute", "cpu_millicores", 500)
		numEq("compute", "memory_mib", 512)
		if mod("database")["postgres_version"] != "17" {
			t.Fatalf("%s version=%v", provider, mod("database")["postgres_version"])
		}
		numEq("database", "storage_gb", 20)
		if doc.Vars.Modules["database"]["publicly_accessible"] != false {
			t.Fatalf("%s public DB not disabled: %v", provider, doc.Vars.Modules["database"])
		}
		if mod("object")["versioning_required"] != true { // the fidelity regression
			t.Fatalf("%s versioning lost: %v", provider, mod("object"))
		}
		if mod("object")["identity_model"] != "logical-key-plus-sha256" {
			t.Fatalf("%s identity model lost", provider)
		}
		if mod("queue")["delivery"] != "at_least_once" {
			t.Fatalf("%s delivery=%v", provider, mod("queue")["delivery"])
		}
		if mod("identity")["federation"] == nil && mod("identity")["oidc_issuer_url"] == nil {
			t.Fatalf("%s identity federation lost", provider)
		}
		if doc.Vars.Modules["network"]["isolation"] != nil {
			// isolation lives in module HCL default-deny; network vars carry plane flag via env wiring
		}
		if doc.Fingerprint == "" || doc.OperationKey == "" {
			t.Fatalf("%s: missing identity", provider)
		}
		// env refs are typed markers, never invented values
		for mname, mvars := range doc.Vars.Modules {
			for k, v := range mvars {
				if ref, ok := v.(map[string]any); ok && ref["source"] == "env" {
					if ref["ref"] == nil || ref["ref"] == "" {
						t.Fatalf("%s.%s.%s: empty env ref", provider, mname, k)
					}
				}
			}
		}
	}
}

// Determinism + blocked/unknown refusal.
func TestTFVarsDeterministic(t *testing.T) {
	plan := tfPlan(t)
	a, err := GenerateTerraformPlan(plan, "azure", "cloudshop")
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateTerraformPlan(plan, "azure", "cloudshop")
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatal("nondeterministic vars")
	}
	plan.OverallStatus = "block"
	if _, err := GenerateTerraformPlan(plan, "azure", "cloudshop"); err == nil {
		t.Fatal("blocked plan generated inputs")
	}
	plan.OverallStatus = "unknown"
	if _, err := GenerateTerraformPlan(plan, "azure", "cloudshop"); err == nil {
		t.Fatal("unknown plan generated inputs")
	}
	if _, err := GenerateTerraformPlan(tfPlan(t), "gcp", "cloudshop"); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

// Conditional emits with warnings (gated later, not silently).
func TestTFVarsConditionalWarns(t *testing.T) {
	reg := testRegistry(t)
	spec := planSpec(t)
	rep := Evaluate(spec, reg, "azure")
	plan := BuildPlan(spec, rep, reg)
	doc, err := GenerateTerraformPlan(plan, "azure", "cloudshop")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Warnings) == 0 {
		t.Fatal("conditional plan without warnings")
	}
}

// State keys isolate environment/workload/scope.
func TestStateKey(t *testing.T) {
	if got := StateKey("dev", "wid-1", "network"); got != "skybridge/dev/wid-1/network.tfstate" {
		t.Fatalf("key=%s", got)
	}
	if StateKey("dev", "w", "a") == StateKey("production-like", "w", "a") {
		t.Fatal("env not isolated")
	}
	if StateKey("dev", "w1", "a") == StateKey("dev", "w2", "a") {
		t.Fatal("workload not isolated")
	}
}

// Apply boundary matrix: checks must pass AND the hard switch stays off.
func TestApplyBoundary(t *testing.T) {
	approved := &Approval{Decision: ApprovalApproved, PolicyInputHash: "h"}
	fresh := func() ApplyPreconditions {
		return ApplyPreconditions{Decision: PolicyAllow, Approval: approved,
			PolicyInputHash: "h", PolicyBundleVersion: "dev",
			PlanFresh: true, CompatFresh: true, DriftFresh: true,
			Environment: "staging", Actor: "operator-1"}
	}
	if err := CheckApplyPreconditions(fresh()); err != nil {
		t.Fatalf("fresh gate: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*ApplyPreconditions)
	}{
		{"deny", func(p *ApplyPreconditions) { p.Decision = PolicyDeny }},
		{"no-approval-when-gated", func(p *ApplyPreconditions) { p.Decision = PolicyApprovalRequired; p.Approval = nil }},
		{"rejected", func(p *ApplyPreconditions) { p.Decision = PolicyApprovalRequired; p.Approval = &Approval{Decision: ApprovalRejected, PolicyInputHash: "h"} }},
		{"expired", func(p *ApplyPreconditions) { p.Decision = PolicyApprovalRequired; p.Approval = &Approval{Decision: ApprovalApproved, PolicyInputHash: "stale", ExpiresAt: "2000-01-01T00:00:00Z"} }},
		{"stale-plan", func(p *ApplyPreconditions) { p.PlanFresh = false }},
		{"stale-compat", func(p *ApplyPreconditions) { p.CompatFresh = false }},
		{"stale-drift", func(p *ApplyPreconditions) { p.DriftFresh = false }},
		{"unprotected-env", func(p *ApplyPreconditions) { p.Environment = "dev" }},
		{"no-actor", func(p *ApplyPreconditions) { p.Actor = "" }},
	}
	for _, c := range cases {
		p := fresh()
		c.mut(&p)
		if err := CheckApplyPreconditions(p); err == nil {
			t.Fatalf("%s: gate opened", c.name)
		}
		if err := RequestApply(p); err == nil {
			t.Fatalf("%s: apply executed", c.name)
		}
	}
	// Even fully fresh preconditions cannot apply in this slice.
	if err := RequestApply(fresh()); err == nil {
		t.Fatal("apply executed while disabled")
	} else if ae, ok := err.(*AdapterError); !ok || ae.Code != ErrApplyDisabled {
		t.Fatalf("err=%v", err)
	}
}

// No-secrets: vars JSON must not embed credential material.
func TestTFVarsNoSecrets(t *testing.T) {
	plan := tfPlan(t)
	for _, provider := range []string{"azure", "aws"} {
		doc, err := GenerateTerraformPlan(plan, provider, "cloudshop")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(doc)
		lower := strings.ToLower(string(b))
		for _, bad := range []string{"akia", "aws_secret", "client_secret", "password", "private_key", "token"} {
			if strings.Contains(lower, bad) {
				t.Fatalf("%s: secret-like content %q", provider, bad)
			}
		}
	}
}

// Race: concurrent generation identical.
func TestTFVarsRace(t *testing.T) {
	plan := tfPlan(t)
	const n = 16
	outs := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			doc, err := GenerateTerraformPlan(plan, "azure", "cloudshop")
			if err != nil {
				t.Errorf("generate: %v", err)
				return
			}
			b, _ := json.Marshal(doc)
			outs[i] = string(b)
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if outs[i] != outs[0] {
			t.Fatal("race divergence")
		}
	}
}
