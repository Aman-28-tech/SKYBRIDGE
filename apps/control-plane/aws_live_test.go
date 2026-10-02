// AWS live-identity gate tests: live reads fail closed without sandbox
// identity and never attempt network (run: go test ./...).
package main

import (
	"context"
	"testing"
	"time"
)

func authorizedAWSReq() ProvisioningRequest {
	return ProvisioningRequest{
		WorkloadID: "w", MigrationID: "m", Provider: "aws",
		Plan: TargetMigrationPlan{ID: "p"}, PolicyInputHash: "h",
		OperationKey: "op-1", Authorized: true,
	}
}

// No identity configured -> typed refusal, fast, no network.
func TestAWSLiveDescribeFailClosed(t *testing.T) {
	t.Setenv("SKYBRIDGE_AWS_ROLE_ARN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ROLE_ARN", "")
	a, _ := SelectAdapter("aws")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err := a.(awsAdapter).DescribeLiveEnvironment(ctx, authorizedAWSReq())
	if err == nil {
		t.Fatal("live describe must refuse without identity")
	}
	if ae, ok := err.(*AdapterError); !ok || ae.Code != ErrAuth {
		t.Fatalf("typed auth error required, got %v", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatal("refusal must not attempt network (too slow)")
	}
}

// Unauthorized requests are refused before identity is even considered.
func TestAWSLiveDescribeRequiresAuth(t *testing.T) {
	a, _ := SelectAdapter("aws")
	req := authorizedAWSReq()
	req.Authorized = false
	_, err := a.(awsAdapter).DescribeLiveEnvironment(context.Background(), req)
	if err == nil {
		t.Fatal("unauthorized live describe must fail")
	}
}

// Explicit sandbox identity resolves (reads still gated behind approval).
func TestAWSLiveIdentityResolution(t *testing.T) {
	t.Setenv("SKYBRIDGE_AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/SKYBRIDGE-dev-test")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ROLE_ARN", "")
	id, err := awsLiveIdentity()
	if err != nil || id == "" {
		t.Fatalf("identity: %q %v", id, err)
	}
	a, _ := SelectAdapter("aws")
	_, err = a.(awsAdapter).DescribeLiveEnvironment(context.Background(), authorizedAWSReq())
	if err == nil {
		t.Fatal("live reads must stay gated even with identity")
	}
}
