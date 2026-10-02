package cdc

import (
	"errors"
	"testing"
)

func TestLSNParseCompare(t *testing.T) {
	a, err := ParseLSN("0/16B1978")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseLSN("0/16B1990")
	if err != nil {
		t.Fatal(err)
	}
	if !(a < b) {
		t.Fatal("LSN order wrong")
	}
	cmp, err := CompareLSN("0/16B1978", "0/16B1990")
	if err != nil || cmp != -1 {
		t.Fatalf("cmp=%d err=%v", cmp, err)
	}
	if _, err := ParseLSN("bogus"); err == nil {
		t.Fatal("malformed LSN must error")
	}
	// Arrival time never orders: equal LSNs compare equal regardless of input order.
	cmp, err = CompareLSN("0/ABC", "0/ABC")
	if err != nil || cmp != 0 {
		t.Fatalf("equal LSN: cmp=%d err=%v", cmp, err)
	}
}

func TestCheckpointSaveReload(t *testing.T) {
	o := NewMemoryCheckpointStore()
	if err := o.Save(Checkpoint{Consumer: "c", SourceLSN: "0/10", Offset: 3}); err != nil {
		t.Fatal(err)
	}
	cp, err := o.Load("c")
	if err != nil {
		t.Fatal(err)
	}
	if cp.SourceLSN != "0/10" || cp.Offset != 3 {
		t.Fatalf("reload mismatch: %+v", cp)
	}
}

func TestCheckpointNeverRewinds(t *testing.T) {
	o := NewMemoryCheckpointStore()
	_ = o.Save(Checkpoint{Consumer: "c", SourceLSN: "0/20"})
	_ = o.Save(Checkpoint{Consumer: "c", SourceLSN: "0/10"}) // stale replay
	cp, _ := o.Load("c")
	if cp.SourceLSN != "0/20" {
		t.Fatalf("checkpoint rewound: %+v", cp)
	}
}

func TestRestartResume(t *testing.T) {
	s := NewMemoryStore()
	o := NewMemoryCheckpointStore()
	m := NewCounters()
	en := NewEngine("c", s, o, m)
	e1 := testUserInsert("0/1", "T1")
	if _, err := en.Apply(e1, 1000, 1005); err != nil {
		t.Fatal(err)
	}
	// Simulate process restart: new engine over the same durable stores.
	en2 := NewEngine("c", s, o, NewCounters())
	// Duplicate redelivery after restart dedupes.
	applied, err := en2.Apply(e1, 1000, 1010)
	if err != nil || applied {
		t.Fatalf("post-restart duplicate: applied=%v err=%v", applied, err)
	}
	// New work resumes from checkpoint.
	e2 := testUserInsert("0/2", "T2")
	e2.PK = "22222222-2222-4222-8222-222222222222"
	e2.After = map[string]any{"id": e2.PK, "email": "b@example.com"}
	applied, err = en2.Apply(e2, 1001, 1011)
	if err != nil || !applied {
		t.Fatalf("resume failed: %v %v", applied, err)
	}
	cp, _ := o.Load("c")
	if cp.SourceLSN != "0/2" {
		t.Fatalf("checkpoint: %+v", cp)
	}
}

func TestFailureBeforeCheckpoint(t *testing.T) {
	s := NewMemoryStore()
	o := NewMemoryCheckpointStore()
	en := NewEngine("c", s, o, NewCounters())
	s.InjectFailure(errors.New("db down"))
	e := testUserInsert("0/1", "T1")
	if _, err := en.Apply(e, 1000, 1005); err == nil {
		t.Fatal("expected failure")
	}
	cp, _ := o.Load("c")
	if cp.SourceLSN != "" {
		t.Fatalf("checkpoint must not advance on failure: %+v", cp)
	}
}

func TestDuplicateAfterRestartNoDoubleApply(t *testing.T) {
	s := NewMemoryStore()
	o := NewMemoryCheckpointStore()
	en := NewEngine("c", s, o, NewCounters())
	e := testUserInsert("0/1", "T1")
	if _, err := en.Apply(e, 1000, 1005); err != nil {
		t.Fatal(err)
	}
	before := s.AppliedCount()
	en2 := NewEngine("c", s, o, NewCounters())
	for i := 0; i < 5; i++ {
		if applied, err := en2.Apply(e, 1000, 1010); err != nil || applied {
			t.Fatalf("replay %d: %v %v", i, applied, err)
		}
	}
	if got := s.AppliedCount(); got != before {
		t.Fatal("duplicate after restart corrupted state")
	}
}

func TestInOrderAcrossTransactions(t *testing.T) {
	en, _, o, _ := engineForTest()
	for i, lsn := range []string{"0/10", "0/11", "0/12"} {
		e := testUserInsert(lsn, string(rune('A'+i)))
		e.PK = "00000000-0000-4000-8000-00000000000" + string(rune('1'+i))
		e.After = map[string]any{"id": e.PK, "email": "x@example.com"}
		if _, err := en.Apply(e, 1000, 1005); err != nil {
			t.Fatalf("lsn %s: %v", lsn, err)
		}
	}
	cp, _ := o.Load("test-consumer")
	if cp.SourceLSN != "0/12" || cp.Offset != 3 {
		t.Fatalf("checkpoint: %+v", cp)
	}
}

func TestOutOfOrderRejected(t *testing.T) {
	en, _, _, _ := engineForTest()
	e2 := testUserInsert("0/20", "T2")
	e2.PK = "22222222-2222-4222-8222-222222222222"
	e2.After = map[string]any{"id": e2.PK, "email": "b@example.com"}
	if _, err := en.Apply(e2, 1000, 1005); err != nil {
		t.Fatal(err)
	}
	late := testUserInsert("0/10", "T1") // older LSN, never applied
	if _, err := en.Apply(late, 999, 1006); err == nil {
		t.Fatal("late out-of-order event must fail explicitly, not corrupt")
	} else if !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("wrong error: %v", err)
	}
}
