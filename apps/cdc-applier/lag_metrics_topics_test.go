package cdc

import (
	"strings"
	"testing"
)

func TestLagSampleRPO(t *testing.T) {
	ok := LagSample{SourceLSN: "0/10", AppliedLSN: "0/10", SourceCommitUnix: 1000, ObserveUnix: 1029}
	if ok.Lag() != 29 || !ok.MeetsRPO() {
		t.Fatalf("29s must meet RPO: %+v", ok)
	}
	breach := LagSample{SourceLSN: "0/10", AppliedLSN: "0/9", SourceCommitUnix: 1000, ObserveUnix: 1031}
	if breach.Lag() != 31 || breach.MeetsRPO() {
		t.Fatalf("31s must breach RPO: %+v", breach)
	}
	skew := LagSample{SourceCommitUnix: 1005, ObserveUnix: 1000}
	if skew.Lag() != 0 || !skew.MeetsRPO() {
		t.Fatal("clock skew clamps to 0/within-RPO")
	}
}

func TestWithinRPO30Boundary(t *testing.T) {
	if !WithinRPO(1000, 1030, 30) {
		t.Fatal("exactly 30s is within RPO")
	}
	if WithinRPO(1000, 1031, 30) {
		t.Fatal("31s breaches")
	}
}

func TestMetricsSnapshot(t *testing.T) {
	en, _, _, m := engineForTest()
	e := testUserInsert("0/1", "T1")
	if _, err := en.Apply(e, 1000, 1020); err != nil {
		t.Fatal(err)
	}
	snap := m.Snapshot()
	if snap.Applied != 1 || snap.Received != 1 {
		t.Fatalf("counters: %+v", snap)
	}
	if snap.LagSeconds != 20 {
		t.Fatalf("lag: %+v", snap)
	}
	if snap.CheckpointLSN != "0/1" {
		t.Fatalf("checkpoint: %+v", snap)
	}
	if snap.CheckpointAgeSec < 0 {
		t.Fatalf("age: %+v", snap)
	}
}

func TestTopicMapping(t *testing.T) {
	for tbl, want := range map[string]string{
		"users": "cloudshop.public.users", "products": "cloudshop.public.products",
		"orders": "cloudshop.public.orders", "order_items": "cloudshop.public.order_items",
	} {
		got, err := TopicForTable(tbl)
		if err != nil || got != want {
			t.Fatalf("%s -> %q, %v", tbl, got, err)
		}
		back, err := TableForTopic(want)
		if err != nil || back != tbl {
			t.Fatalf("inverse %s: %q %v", want, back, err)
		}
	}
	if got := ConnectorTableList(); strings.Contains(got, "idempotency_keys") {
		t.Fatalf("connector must exclude idempotency_keys: %q", got)
	}
	for _, ex := range ExcludedTables {
		if _, err := TopicForTable(ex); err == nil {
			t.Fatalf("excluded %q must not map", ex)
		}
	}
	if len(BusinessTopics) != 4 {
		t.Fatalf("topic proliferation: %v", BusinessTopics)
	}
}

func TestSourceTableFilteringContract(t *testing.T) {
	// Connector config + code must agree: only the 4 business tables flow.
	want := "public.users,public.products,public.orders,public.order_items"
	if got := ConnectorTableList(); got != want {
		t.Fatalf("connector list: %q want %q", got, want)
	}
}
