// Local multi-cloud demo fixtures: deterministic AWS/Azure observed state
// aligned with the adapter translators (run: go test ./...). Fixtures carry
// identity/endpoint facts only; drift-relevant config must match the plan.
package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func loadDemoFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile("../../tests/fixtures/demo/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func demoPlan(t *testing.T, wid string) TargetMigrationPlan {
	t.Helper()
	plan, ok := store.GetLatestTargetPlan(wid)
	if !ok {
		t.Fatal("no plan")
	}
	return plan
}

func demoChain(t *testing.T, key string) string {
	t.Helper()
	resetStore()
	wid := registerPlanWID(t, key+"reg00001")
	evalCompat(t, wid, key+"cmp00001")
	evalPlanForDrift(t, wid)
	return wid
}

// Fixtures: provider identity, standby/source authority, no secrets.
func TestDemoFixturesShape(t *testing.T) {
	aws := loadDemoFixture(t, "aws-observed")
	azure := loadDemoFixture(t, "azure-observed")
	if aws["provider"] != "aws" || aws["authority"] != "authoritative" {
		t.Fatalf("aws: %+v", aws["provider"])
	}
	if azure["provider"] != "azure" || azure["authority"] != "standby" {
		t.Fatalf("azure: %+v", azure["provider"])
	}
	for name, fx := range map[string]map[string]any{"aws": aws, "azure": azure} {
		b, _ := json.Marshal(fx)
		lower := strings.ToLower(string(b))
		for _, bad := range []string{"password", "secret", "akia", "private_key", "connection_string", "client_secret"} {
			if strings.Contains(lower, bad) {
				t.Fatalf("%s fixture leaks %q", name, bad)
			}
		}
		if !strings.Contains(string(b), "example.invalid") {
			t.Fatalf("%s fixture must use example.invalid endpoints", name)
		}
		comps, _ := fx["components"].(map[string]any)
		for _, k := range []string{"compute", "database", "cache", "object-storage", "queue", "identity", "network", "routing", "observability", "security"} {
			if _, ok := comps[k]; !ok {
				t.Fatalf("%s fixture lacks component %q", name, k)
			}
		}
		// Alignment with the canonical plan: versions must agree.
		db, _ := comps["database"].(map[string]any)
		if db["engine"] != "postgresql" || db["major_version"] != "17" || db["storage_gib"] != float64(20) {
			t.Fatalf("%s database facts skewed: %+v", name, db)
		}
	}
}

// Adapters run Validate/Plan/Describe/Verify over fixture-backed plans,
// always reporting LOCAL_FIXTURE and never touching the network.
func TestDemoAdaptersOverFixtures(t *testing.T) {
	wid := demoChain(t, "demofixture00001")
	plan := demoPlan(t, wid)
	for _, provider := range []string{"aws", "azure"} {
		a, err := SelectAdapter(provider)
		if err != nil {
			t.Fatal(err)
		}
		req := NewProvisioningRequest(plan, provider, wid, "m-demo", "wf-demo", "hash-demo", "")
		req.Authorized = true
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		start := time.Now()
		if err := a.ValidateTarget(ctx, req); err != nil {
			t.Fatalf("%s validate: %v", provider, err)
		}
		pp, err := a.PlanInfrastructure(ctx, req)
		if err != nil {
			t.Fatalf("%s plan: %v", provider, err)
		}
		desc, err := a.DescribeInfrastructure(ctx, req)
		if err != nil {
			t.Fatalf("%s describe: %v", provider, err)
		}
		if desc["source_of_truth"] != "LOCAL_FIXTURE" {
			t.Fatalf("%s source_of_truth: %+v", provider, desc)
		}
		if desc["fingerprint"] != pp.Fingerprint {
			t.Fatalf("%s fingerprint: %+v", provider, desc)
		}
		ver, err := a.VerifyInfrastructure(ctx, pp)
		if err != nil || !ver.Verified {
			t.Fatalf("%s verify: %v %+v", provider, err, ver)
		}
		cancel()
		if time.Since(start) > 15*time.Second {
			t.Fatalf("%s touched the network (too slow)", provider)
		}
		if err := a.ApplyInfrastructure(ctx, req); err == nil {
			t.Fatalf("%s apply must refuse", provider)
		}
	}
}

// Fixture facts build a clean drift snapshot; a tampered fact blocks.
func TestDemoDriftFromFixtures(t *testing.T) {
	wid := demoChain(t, "demodrift0000001")
	plan := demoPlan(t, wid)
	azure := loadDemoFixture(t, "azure-observed")
	fxComps, _ := azure["components"].(map[string]any)

	snapshot := func() map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, c := range plan.Components {
			cfg := map[string]any{}
			b, _ := json.Marshal(c.DesiredConfiguration)
			_ = json.Unmarshal(b, &cfg)
			// Documentary fixture facts (resource_id/endpoint/status) ride
			// along as informational unmodeled fields; drift compares
			// config keys only.
			if fx, ok := fxComps[c.Key].(map[string]any); ok {
				for _, k := range []string{"resource_id", "endpoint", "status"} {
					if v, ok := fx[k]; ok {
						cfg[k] = v
					}
				}
			}
			// Conditional components carry a validation gate: the demo
			// records the gate satisfied out-of-band, exactly as the
			// remediation hint prescribes.
			if c.ValidationGate != "" {
				cfg["prerequisite_validated"] = true
			}
			out[c.Key] = cfg
		}
		return out
	}

	clean := EvaluateDrift(plan, ObservedSnapshot{
		SnapshotVersion: 1, ObservedAt: time.Now().UTC().Format(time.RFC3339),
		Components:      snapshot(),
	})
	for _, f := range clean.Findings {
		if f.Severity == "blocking" || f.Severity == "security_critical" {
			t.Fatalf("clean snapshot must not block: %+v", f)
		}
	}
	if clean.DriftGate != "clear" {
		t.Fatalf("clean snapshot gate=%s", clean.DriftGate)
	}

	bad := snapshot()
	db := bad["database"]
	db["major_version"] = "16" // tampered provider fact
	rep := EvaluateDrift(plan, ObservedSnapshot{
		SnapshotVersion: 1, ObservedAt: time.Now().UTC().Format(time.RFC3339),
		Components:      bad,
	})
	found := false
	for _, f := range rep.Findings {
		if f.Path == "database.major_version" && f.Severity == "blocking" {
			found = true
		}
	}
	if !found || rep.DriftGate != "blocking" {
		t.Fatalf("tampered fact must block: gate=%s findings=%+v", rep.DriftGate, rep.Findings)
	}
}
