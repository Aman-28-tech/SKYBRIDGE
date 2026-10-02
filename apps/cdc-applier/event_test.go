package cdc

import (
	"strings"
	"testing"
)

func testUserInsert(lsn, txn string) *CDCEvent {
	return &CDCEvent{
		WorkloadID: "cloudshop", SourceDB: "cloudshop", Table: "users",
		Op: "c", PK: "11111111-1111-4111-8111-111111111111",
		LSN: lsn, SourceCommitMs: 1700000000000, TxnID: txn,
		SchemaVersion: 1,
		After:         map[string]any{"id": "11111111-1111-4111-8111-111111111111", "email": "a@example.com"},
	}
}

func TestEventSerializationRoundTrip(t *testing.T) {
	e := testUserInsert("0/16B1978", "512")
	b, err := e.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	rt, err := UnmarshalCDCEvent(b)
	if err != nil {
		t.Fatal(err)
	}
	if rt.EventID() != e.EventID() {
		t.Fatal("round-trip changed EventID")
	}
	if rt.PK != e.PK || rt.LSN != e.LSN || rt.Op != e.Op {
		t.Fatal("round-trip field mismatch")
	}
}

func TestEventIDDeterminism(t *testing.T) {
	a := testUserInsert("0/16B1978", "512")
	b := testUserInsert("0/16B1978", "512")
	if a.EventID() != b.EventID() {
		t.Fatal("same source identity must give same EventID")
	}
	c := testUserInsert("0/16B1990", "512") // new LSN == new event
	if c.EventID() == a.EventID() {
		t.Fatal("different LSN must give different EventID")
	}
	d := testUserInsert("0/16B1978", "999") // different txn == different ID
	if d.EventID() == a.EventID() {
		t.Fatal("different txn must give different EventID")
	}
}

func TestEventIDIgnoresWallClock(t *testing.T) {
	a := testUserInsert("0/16B1978", "512")
	b := testUserInsert("0/16B1978", "512")
	a.SourceCommitMs = 1700000000000
	b.SourceCommitMs = 1799999999999 // wall-clock must not enter identity
	// EventID intentionally excludes SourceCommitMs; assert stability.
	if a.EventID() != b.EventID() {
		t.Fatal("EventID must not depend on wall-clock commit ms")
	}
}

func TestMalformedRejection(t *testing.T) {
	for _, bad := range []string{
		`not json`,
		`{"workload_id":"x"}`,
		`{"workload_id":"w","source_db":"d","table":"users","op":"c","pk":"","lsn":"0/1","source_commit_ms":1,"txn_id":"1","schema_version":1,"after":{"id":"x"}}`,
		`{"workload_id":"w","source_db":"d","table":"users","op":"c","pk":"x","lsn":"","source_commit_ms":1,"txn_id":"1","schema_version":1,"after":{"id":"x"}}`,
	} {
		if _, err := UnmarshalCDCEvent([]byte(bad)); err == nil {
			t.Fatalf("expected rejection for %q", bad)
		}
	}
	// Unknown fields rejected (strict decoding).
	e := testUserInsert("0/1", "1")
	b, _ := e.MarshalCanonical()
	withExtra := strings.Replace(string(b), `"pk"`, `"pk_extra":1,"pk"`, 1)
	if _, err := UnmarshalCDCEvent([]byte(withExtra)); err == nil {
		t.Fatal("unknown fields must be rejected")
	}
}

func TestUnknownOpRejection(t *testing.T) {
	e := testUserInsert("0/1", "1")
	e.Op = "x"
	if err := e.Validate(); err == nil {
		t.Fatal("unknown op must be rejected")
	}
	e.Op = "t" // truncate-style op must also be rejected in v1
	if err := e.Validate(); err == nil {
		t.Fatal("truncate op must be rejected in v1")
	}
}

func TestTableFiltering(t *testing.T) {
	for _, tbl := range []string{"idempotency_keys", "applied_jobs", "schema_version", "cdc_offsets", "redis"} {
		e := testUserInsert("0/1", "1")
		e.Table = tbl
		if err := e.Validate(); err == nil {
			t.Fatalf("infrastructure table %q must not validate", tbl)
		}
	}
}

func TestFromDebeziumMapping(t *testing.T) {
	rec := `{"before":null,"after":{"id":"11111111-1111-4111-8111-111111111111","email":"a@example.com"},` +
		`"source":{"db":"cloudshop","table":"users","lsn":"0/16B1978","txId":512,"ts_ms":1700000000000},` +
		`"op":"c","ts_ms":1700000000500,"transaction":{"id":"512:0/16B1978"}}`
	e, err := FromDebezium("cloudshop", []byte(rec))
	if err != nil {
		t.Fatal(err)
	}
	if e.Table != "users" || e.PK != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("bad mapping: %+v", e)
	}
	if e.LSN != "0/16B1978" || e.SourceCommitMs != 1700000000000 {
		t.Fatalf("must use source.lsn/source.ts_ms, got %+v", e)
	}
	if e.TxnID == "" {
		t.Fatal("txn identity must be captured")
	}
	// Envelope ts_ms (processing time) must not leak into the event.
	if e.SourceCommitMs == 1700000000500 {
		t.Fatal("envelope ts_ms must not be used as commit time")
	}
}

func TestFromDebeziumDelete(t *testing.T) {
	rec := `{"before":{"id":"11111111-1111-4111-8111-111111111111","email":"a@example.com"},"after":null,` +
		`"source":{"db":"cloudshop","table":"users","lsn":"0/16B2000","txId":513,"ts_ms":1700000001000},` +
		`"op":"d","ts_ms":1700000001500}`
	e, err := FromDebezium("cloudshop", []byte(rec))
	if err != nil {
		t.Fatal(err)
	}
	if e.Op != "d" || e.PK == "" {
		t.Fatalf("delete must carry PK from before: %+v", e)
	}
}

func TestFromDebeziumMalformed(t *testing.T) {
	for _, bad := range []string{`{}`, `{"op":"c"}`, `not-json`} {
		if _, err := FromDebezium("cloudshop", []byte(bad)); err == nil {
			t.Fatalf("expected mapping failure for %q", bad)
		}
	}
}

func TestFromDebeziumNumericLSN(t *testing.T) {
	// Live pgoutput sends source.lsn as a JSON number (observed 26812488).
	rec := `{"before":null,"after":{"id":"11111111-1111-4111-8111-111111111111","email":"a@example.com"},` +
		`"source":{"db":"cloudshop","table":"users","lsn":26812488,"txId":758,"ts_ms":1700000000000},` +
		`"op":"c","ts_ms":1700000000500}`
	e, err := FromDebezium("cloudshop", []byte(rec))
	if err != nil {
		t.Fatal(err)
	}
	if e.LSN != "0/1992048" {
		t.Fatalf("numeric LSN not normalized: %q", e.LSN)
	}
	if _, err := ParseLSN(e.LSN); err != nil {
		t.Fatalf("normalized LSN must parse: %v", err)
	}
	cmp, err := CompareLSN("0/1992048", "0/1992049")
	if err != nil || cmp != -1 {
		t.Fatalf("ordering on normalized LSNs: %d %v", cmp, err)
	}
}
