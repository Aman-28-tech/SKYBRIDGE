// Adapter contract tests: translation fidelity, safety boundaries,
// determinism, and mock integration (no cloud, no apply, no secrets).
package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func adapterPlan(t *testing.T) TargetMigrationPlan {
	t.Helper()
	reg := fullPassRegistry(t)
	spec := planSpec(t)
	rep := Evaluate(spec, reg, "azure")
	plan := BuildPlan(spec, rep, reg)
	if plan.OverallStatus != "pass" {
		t.Fatalf("setup: %s", plan.OverallStatus)
	}
	return plan
}

func authorizedReq(plan TargetMigrationPlan, provider string) ProvisioningRequest {
	req := NewProvisioningRequest(plan, provider, "wid-1", "mig-1", "wf-1", "hash-abc", "")
	req.Authorized = true
	return req
}

func intentByKey(p ProvisioningPlan, key string) ResourceIntent {
	for _, r := range p.Resources {
		if strings.Contains(r.LogicalID, ":"+key+":") {
			return r
		}
	}
	return ResourceIntent{}
}

// Fidelity: every material plan field survives translation, both providers.
func TestAdapterFidelity(t *testing.T) {
	plan := adapterPlan(t)
	for _, provider := range []string{"azure", "aws"} {
		a, err := SelectAdapter(provider)
		if err != nil {
			t.Fatal(err)
		}
		pp, err := a.PlanInfrastructure(context.Background(), authorizedReq(plan, provider))
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		if len(pp.Resources) != 10 {
			t.Fatalf("%s: resources=%d, want 10 (11 components minus derived data-movement)", provider, len(pp.Resources))
		}
		numEq := func(key, field string, want float64) {
			t.Helper()
			got, ok := num(intentByKey(pp, key).Desired[field])
			if !ok || got != want {
				t.Fatalf("%s %s.%s=%v want %v", provider, key, field, intentByKey(pp, key).Desired[field], want)
			}
		}
		strEq := func(key, field, want string) {
			t.Helper()
			if intentByKey(pp, key).Desired[field] != want {
				t.Fatalf("%s %s.%s=%v want %v", provider, key, field, intentByKey(pp, key).Desired[field], want)
			}
		}
		boolEq := func(key, field string, want bool) {
			t.Helper()
			if intentByKey(pp, key).Desired[field] != want {
				t.Fatalf("%s %s.%s=%v want %v", provider, key, field, intentByKey(pp, key).Desired[field], want)
			}
		}
		numEq("compute", "replicas", 2)
		numEq("compute", "cpu_millicores", 500)
		numEq("compute", "memory_mib", 512)
		strEq("database", "engine", "postgresql")
		strEq("database", "major_version", "17")
		numEq("database", "storage_gib", 20)
		boolEq("database", "private_networking", true)
		boolEq("cache", "authoritative", false)
		boolEq("object-storage", "required", true)
		boolEq("object-storage", "versioning_required", true) // the fidelity regression
		strEq("object-storage", "identity_model", "logical-key-plus-sha256")
		strEq("queue", "delivery", "at_least_once")
		boolEq("queue", "duplicate_safe_consumer", true)
		boolEq("identity", "least_privilege", true)
		boolEq("network", "private_data_plane", true)
		strEq("network", "isolation", "default-deny")
		boolEq("security", "long_lived_credentials", false)
		if pp.RPOSeconds != 30 || pp.RTOSeconds != 900 {
			t.Fatalf("%s: rpo/rto=%d/%d", provider, pp.RPOSeconds, pp.RTOSeconds)
		}
		if pp.OperationKey == "" || pp.Fingerprint == "" {
			t.Fatalf("%s: missing key/fingerprint", provider)
		}
		if len(pp.Warnings) == 0 {
			t.Fatalf("%s: derived data-movement must be stated, not silent", provider)
		}
		// provider-specific kinds differ; statuses identical
		for _, r := range pp.Resources {
			if r.Provider != provider {
				t.Fatalf("cross-provider leak: %s", r.LogicalID)
			}
		}
	}
	az, _ := SelectAdapter("azure")
	aws, _ := SelectAdapter("aws")
	pa, _ := az.PlanInfrastructure(context.Background(), authorizedReq(plan, "azure"))
	pw, _ := aws.PlanInfrastructure(context.Background(), authorizedReq(plan, "aws"))
	if intentByKey(pa, "compute").Kind == intentByKey(pw, "compute").Kind {
		t.Fatal("providers must map to distinct kinds")
	}
	if intentByKey(pa, "compute").Mode != intentByKey(pw, "compute").Mode {
		t.Fatal("modes must agree across providers")
	}
}

// Unsupported: blocked requirements fail loudly; unknown providers refuse.
func TestAdapterUnsupported(t *testing.T) {
	plan := adapterPlan(t)
	// flip queue to blocked by hand (simulates a blocked compat input)
	for i := range plan.Components {
		if plan.Components[i].Key == "queue" {
			plan.Components[i].ProvisioningMode = ModeBlocked
			plan.Components[i].DesiredConfiguration = map[string]any{}
		}
	}
	a, _ := SelectAdapter("azure")
	if _, err := a.PlanInfrastructure(context.Background(), authorizedReq(plan, "azure")); err == nil {
		t.Fatal("blocked component planned silently")
	} else if ae, ok := err.(*AdapterError); !ok || ae.Code != ErrUnsupported {
		t.Fatalf("err=%v", err)
	}
	if _, err := SelectAdapter("gcp"); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

// Policy boundary: no authorized context, no work. Apply always refuses.
func TestAdapterAuthBoundary(t *testing.T) {
	plan := adapterPlan(t)
	a, _ := SelectAdapter("azure")
	bare := NewProvisioningRequest(plan, "azure", "w", "m", "wf", "h", "")
	if _, err := a.PlanInfrastructure(context.Background(), bare); err == nil {
		t.Fatal("unauthorized planning proceeded")
	} else if ae, ok := err.(*AdapterError); !ok || ae.Code != ErrAuth {
		t.Fatalf("err=%v", err)
	}
	noHash := authorizedReq(plan, "azure")
	noHash.PolicyInputHash = ""
	if err := a.ValidateTarget(context.Background(), noHash); err == nil {
		t.Fatal("hashless request validated")
	}
	okReq := authorizedReq(plan, "azure")
	if err := a.ApplyInfrastructure(context.Background(), okReq); err == nil {
		t.Fatal("apply executed")
	} else if ae, ok := err.(*AdapterError); !ok || ae.Code != ErrApplyDisabled || ae.Retryable {
		t.Fatalf("err=%v", err)
	}
	aws := awsAdapter{}
	if err := aws.ApplyInfrastructure(context.Background(), okReq); err == nil {
		t.Fatal("aws apply executed")
	}
}

// Determinism: same inputs always equal; operation key stable.
func TestAdapterDeterministic(t *testing.T) {
	plan := adapterPlan(t)
	a, _ := SelectAdapter("azure")
	req := authorizedReq(plan, "azure")
	p1, err := a.PlanInfrastructure(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := a.PlanInfrastructure(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	j1, _ := json.Marshal(p1)
	j2, _ := json.Marshal(p2)
	if string(j1) != string(j2) {
		t.Fatal("nondeterministic translation")
	}
	if OperationKey(plan.ID, "azure") != req.OperationKey {
		t.Fatal("operation key unstable")
	}
	ver, err := a.VerifyInfrastructure(context.Background(), p1)
	if err != nil || !ver.Verified || len(ver.Checks) != len(p1.Resources) {
		t.Fatalf("verify: %v %+v", err, ver)
	}
	desc, err := a.DescribeInfrastructure(context.Background(), req)
	if err != nil || desc["fingerprint"] != p1.Fingerprint {
		t.Fatalf("describe: %v", desc)
	}
}

// No credentials: intents must contain no secret material or key names.
func TestAdapterNoSecrets(t *testing.T) {
	plan := adapterPlan(t)
	for _, provider := range []string{"azure", "aws"} {
		a, _ := SelectAdapter(provider)
		pp, err := a.PlanInfrastructure(context.Background(), authorizedReq(plan, provider))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(pp)
		lower := strings.ToLower(string(b))
		for _, bad := range []string{"password", "secret", "api_key", "apikey", "akia", "token", "private_key", "connection_string"} {
			if strings.Contains(lower, bad) {
				t.Fatalf("%s: secret-like content %q in intents", provider, bad)
			}
		}
	}
}

// Race: concurrent translation is safe and identical.
func TestAdapterRace(t *testing.T) {
	plan := adapterPlan(t)
	a, _ := SelectAdapter("azure")
	const n = 16
	outs := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pp, err := a.PlanInfrastructure(context.Background(), authorizedReq(plan, "azure"))
			if err != nil {
				t.Errorf("translate: %v", err)
				return
			}
			b, _ := json.Marshal(pp)
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

// Temporal integration: the activity invokes the adapter and propagates
// provider, fingerprint, and verification into workflow state.
func TestMockProvisionViaAdapter(t *testing.T) {
	resetStore()
	wid, migID := wiredPassSetup(t)
	in := execContextForTest(t, wid, migID, 1)
	acts := &ExecActivities{}
	out, err := acts.MockProvisionPlanActivity(context.Background(), in)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if out["provider"] != "azure" || out["mutated"] != false || out["verified"] != true {
		t.Fatalf("result: %v", out)
	}
	if out["operation_key"] == nil || out["fingerprint"] == nil {
		t.Fatal("missing operation identity")
	}
}
