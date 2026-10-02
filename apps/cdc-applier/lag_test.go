package cdc

import "testing"

func TestLagSeconds(t *testing.T) {
	if got := LagSeconds(1000, 1008); got != 8 {
		t.Fatalf("got %d", got)
	}
	if !WithinRPO(1000, 1008, 30) {
		t.Fatal("8s should be within 30s RPO")
	}
	if WithinRPO(1000, 1031, 30) {
		t.Fatal("31s should breach 30s RPO")
	}
	if !WithinRPO(1000, 999, 30) {
		t.Fatal("negative skew should clamp to within-RPO")
	}
}

func TestAppliedSetDedupe(t *testing.T) {
	a := NewAppliedSet()
	if !a.Apply("txn-1") {
		t.Fatal("first apply must succeed")
	}
	if a.Apply("txn-1") {
		t.Fatal("redelivery must not double-apply")
	}
	if !a.Apply("txn-2") {
		t.Fatal("new txn must apply")
	}
}
