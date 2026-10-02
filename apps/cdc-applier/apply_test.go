package cdc

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func engineForTest() (*Engine, *MemoryStore, *MemoryCheckpointStore, *Counters) {
	s := NewMemoryStore()
	o := NewMemoryCheckpointStore()
	m := NewCounters()
	return NewEngine("test-consumer", s, o, m), s, o, m
}

func orderInsert(pk, user, lsn, txn string) *CDCEvent {
	return &CDCEvent{
		WorkloadID: "cloudshop", SourceDB: "cloudshop", Table: "orders",
		Op: "c", PK: pk, LSN: lsn, SourceCommitMs: 1700000000000, TxnID: txn,
		SchemaVersion: 1,
		After: map[string]any{"id": pk, "user_id": user, "status": "pending", "total_cents": float64(100)},
	}
}

func TestInsertApply(t *testing.T) {
	en, s, _, _ := engineForTest()
	if _, err := s.ApplyAtomically(&CDCEvent{
		WorkloadID: "cloudshop", SourceDB: "cloudshop", Table: "users", Op: "c",
		PK: "u-1", LSN: "0/1", SourceCommitMs: 1, TxnID: "10", SchemaVersion: 1,
		After: map[string]any{"id": "u-1", "email": "u@example.com"},
	}); err != nil {
		t.Fatal(err)
	}
	applied, err := en.Apply(orderInsert("o-1", "u-1", "0/2", "11"), 1700000000000, 1700000000005)
	if err != nil || !applied {
		t.Fatalf("insert failed: applied=%v err=%v", applied, err)
	}
	row, ok := s.Get("orders", "o-1")
	if !ok || row["status"] != "pending" {
		t.Fatalf("row missing: %v", row)
	}
}

func TestUpdateApply(t *testing.T) {
	en, s, _, _ := engineForTest()
	e := testUserInsert("0/1", "10")
	if _, err := en.Apply(e, 1000, 1005); err != nil {
		t.Fatal(err)
	}
	u := testUserInsert("0/2", "11")
	u.Op = "u"
	u.After = map[string]any{"id": u.PK, "email": "b@example.com"}
	applied, err := en.Apply(u, 1000, 1006)
	if err != nil || !applied {
		t.Fatalf("update failed: %v %v", applied, err)
	}
	row, _ := s.Get("users", u.PK)
	if row["email"] != "b@example.com" {
		t.Fatalf("update not reflected: %v", row)
	}
}

func TestDeleteApply(t *testing.T) {
	en, s, _, _ := engineForTest()
	e := testUserInsert("0/1", "10")
	if _, err := en.Apply(e, 1000, 1005); err != nil {
		t.Fatal(err)
	}
	d := &CDCEvent{WorkloadID: "cloudshop", SourceDB: "cloudshop", Table: "users",
		Op: "d", PK: e.PK, LSN: "0/2", SourceCommitMs: 1001, TxnID: "11",
		SchemaVersion: 1, Before: map[string]any{"id": e.PK, "email": "a@example.com"}}
	applied, err := en.Apply(d, 1001, 1006)
	if err != nil || !applied {
		t.Fatalf("delete failed: %v %v", applied, err)
	}
	if _, ok := s.Get("users", e.PK); ok {
		t.Fatal("row must be gone after delete")
	}
	// Delete is idempotent: second delete of same row at new LSN succeeds (0 rows ok).
	d2 := &CDCEvent{WorkloadID: "cloudshop", SourceDB: "cloudshop", Table: "users",
		Op: "d", PK: e.PK, LSN: "0/3", SourceCommitMs: 1002, TxnID: "12",
		SchemaVersion: 1, Before: map[string]any{"id": e.PK, "email": "a@example.com"}}
	if _, err := en.Apply(d2, 1002, 1007); err != nil {
		t.Fatalf("idempotent delete failed: %v", err)
	}
}

func TestReplay100TimesUnchanged(t *testing.T) {
	en, s, _, m := engineForTest()
	e := testUserInsert("0/1", "10")
	applied, err := en.Apply(e, 1000, 1005)
	if err != nil || !applied {
		t.Fatalf("first apply: %v %v", applied, err)
	}
	for i := 0; i < 100; i++ {
		dup, err := en.Apply(e, 1000, 1005+int64(i))
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if dup {
			t.Fatalf("replay %d must not re-apply", i)
		}
	}
	row, ok := s.Get("users", e.PK)
	if !ok || row["email"] != "a@example.com" {
		t.Fatalf("state corrupted by replay: %v", row)
	}
	if got := s.AppliedCount(); got != 1 {
		t.Fatalf("expected 1 applied event, got %d", got)
	}
	if snap := m.Snapshot(); snap.Duplicates != 100 {
		t.Fatalf("expected 100 duplicates, got %+v", snap)
	}
}

func TestSameTransactionMultipleEvents(t *testing.T) {
	// T1 = E1(users) + E2(orders) + E3(order_items). All three must apply even
	// though they share TxnID: txn marker must never skip E2/E3.
	en, s, _, _ := engineForTest()
	u := testUserInsert("0/1", "T1")
	p := &CDCEvent{WorkloadID: "cloudshop", SourceDB: "cloudshop", Table: "products",
		Op: "c", PK: "p-1", LSN: "0/2", SourceCommitMs: 1000, TxnID: "T1",
		SchemaVersion: 1, After: map[string]any{"id": "p-1", "sku": "SKU-1", "name": "W", "price_cents": float64(999)}}
	o := orderInsert("o-1", u.PK, "0/3", "T1")
	for i, e := range []*CDCEvent{u, p, o} {
		applied, err := en.Apply(e, 1000, 1005)
		if err != nil || !applied {
			t.Fatalf("E%d failed: applied=%v err=%v", i+1, applied, err)
		}
	}
	if _, ok := s.Get("users", u.PK); !ok {
		t.Fatal("E1 missing")
	}
	if _, ok := s.Get("products", "p-1"); !ok {
		t.Fatal("E2 missing: txn marker must not skip same-txn events")
	}
	if _, ok := s.Get("orders", "o-1"); !ok {
		t.Fatal("E3 missing: txn marker must not skip same-txn events")
	}
}

func TestRollbackOnFailure(t *testing.T) {
	en, s, _, _ := engineForTest()
	s.InjectFailure(errors.New("transient target failure"))
	e := testUserInsert("0/1", "10")
	if _, err := en.Apply(e, 1000, 1005); err == nil {
		t.Fatal("expected failure")
	}
	if _, ok := s.Get("users", e.PK); ok {
		t.Fatal("failed event must not partially apply")
	}
	dup, err := s.HasApplied(e.EventID())
	if err != nil || dup {
		t.Fatal("failed event must not leave a dedupe marker")
	}
	// Retry after recovery succeeds.
	applied, err := en.Apply(e, 1000, 1006)
	if err != nil || !applied {
		t.Fatalf("retry failed: %v %v", applied, err)
	}
}

func TestConcurrentApplySameEvent(t *testing.T) {
	en, s, _, m := engineForTest()
	e := testUserInsert("0/1", "10")
	var wg sync.WaitGroup
	results := make([]bool, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			applied, _ := en.Apply(e, 1000, 1005)
			results[i] = applied
		}(i)
	}
	wg.Wait()
	n := 0
	for _, r := range results {
		if r {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("exactly one concurrent apply must win, got %d", n)
	}
	if got := s.AppliedCount(); got != 1 {
		t.Fatalf("store has %d events, want 1", got)
	}
	if snap := m.Snapshot(); snap.Applied != 1 {
		t.Fatalf("metrics: %+v", snap)
	}
}

func TestBuildApplySQLTouchesRealSchema(t *testing.T) {
	// Guards against generic SQL hiding schema errors.
	cases := map[string]map[string]any{
		"users":       {"id": "x", "email": "e@x.com"},
		"products":    {"id": "x", "sku": "s", "name": "n", "price_cents": float64(1)},
		"orders":      {"id": "x", "user_id": "u", "status": "pending", "total_cents": float64(1)},
		"order_items": {"id": "x", "order_id": "o", "product_id": "p", "quantity": float64(1), "unit_price_cents": float64(1)},
	}
	for tbl, after := range cases {
		e := &CDCEvent{WorkloadID: "w", SourceDB: "d", Table: tbl, Op: "c",
			PK: "x", LSN: "0/1", SourceCommitMs: 1, TxnID: "1", SchemaVersion: 1, After: after}
		primary, _, err := BuildApplySQL(e)
		if err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
		if primary.Statement == "" || len(primary.Args) == 0 {
			t.Fatalf("%s: empty SQL", tbl)
		}
	}
	// Invalid status / quantity surface as errors, not silent SQL.
	bad := &CDCEvent{WorkloadID: "w", SourceDB: "d", Table: "orders", Op: "c",
		PK: "x", LSN: "0/1", SourceCommitMs: 1, TxnID: "1", SchemaVersion: 1,
		After: map[string]any{"id": "x", "user_id": "u", "status": "bogus", "total_cents": float64(1)}}
	if _, _, err := BuildApplySQL(bad); err == nil {
		t.Fatal("invalid status must error")
	}
}

func TestTargetSchemaAvailability(t *testing.T) {
	// Documents the contract the live target must satisfy.
	required := []string{"users", "products", "orders", "order_items"}
	s := NewMemoryStore()
	for _, tbl := range required {
		if _, ok := s.rows[tbl]; !ok {
			t.Fatalf("target missing table %s", tbl)
		}
	}
	for _, tbl := range []string{"idempotency_keys", "cdc_offsets"} {
		if _, ok := s.rows[tbl]; ok {
			t.Fatalf("business store must not own %s", tbl)
		}
	}
}

func ExampleBuildApplySQL() {
	e := testUserInsert("0/1", "1")
	primary, _, _ := BuildApplySQL(e)
	fmt.Println(primary.Statement[:21])
	// Output: INSERT INTO users (id
}
