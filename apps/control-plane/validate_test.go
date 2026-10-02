// Regression tests for server-side canonical schema validation (FR-004).
// The schema at schemas/canonical-workload.schema.json must stay
// byte-identical to packages/contracts/schemas/canonical-workload.schema.json.
package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"
)

func mustSpec(t *testing.T, mutate func(m map[string]any)) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(fullSpecBody()), &m); err != nil {
		t.Fatal(err)
	}
	mutate(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func postWorkload(key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	withBearer(req, "test-admin", "")
	rec := httptest.NewRecorder()
	registerWorkload(rec, req)
	return rec
}

func TestEmbeddedSchemaMatchesCanonical(t *testing.T) {
	canon, err := os.ReadFile("../../packages/contracts/schemas/canonical-workload.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(canon) != string(canonicalSchemaBytes) {
		t.Fatal("embedded schema copy drifted from canonical file; re-copy it")
	}
	if _, err := canonicalSchema(); err != nil {
		t.Fatalf("schema does not compile: %v", err)
	}
}

// A: valid workload passes.
func TestValidationAcceptsValid(t *testing.T) {
	var v any
	_ = json.Unmarshal([]byte(fullSpecBody()), &v)
	if failures := ValidateCanonical(v); failures != nil {
		t.Fatalf("valid spec rejected: %+v", failures)
	}
}

// B: missing required top-level field fails.
func TestValidationMissingField(t *testing.T) {
	body := mustSpec(t, func(m map[string]any) { delete(m, "queue") })
	var v any
	_ = json.Unmarshal([]byte(body), &v)
	if failures := ValidateCanonical(v); len(failures) == 0 {
		t.Fatal("missing required field accepted")
	}
}

// C: wrong field type fails.
func TestValidationWrongType(t *testing.T) {
	body := mustSpec(t, func(m map[string]any) {
		m["compute"].(map[string]any)["replicas"] = "two"
	})
	var v any
	_ = json.Unmarshal([]byte(body), &v)
	if failures := ValidateCanonical(v); len(failures) == 0 {
		t.Fatal("wrong type accepted")
	}
}

// D + schema-driven proof: violations only the real schema catches —
// const enum, numeric minimum, and additionalProperties at root and nested.
// A hand-written required-fields check would accept all of these.
func TestValidationSchemaDriven(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"const-enum": func(m map[string]any) {
			m["compute"].(map[string]any)["orchestrator"] = "nomad"
		},
		"minimum": func(m map[string]any) {
			m["compute"].(map[string]any)["cpu_millicores"] = 50 // minimum 100
		},
		"root-additional": func(m map[string]any) {
			m["extra_unknown"] = true // root additionalProperties: false
		},
		"nested-additional": func(m map[string]any) {
			m["cache"].(map[string]any)["ttl_seconds"] = 60 // cache additionalProperties: false
		},
		"pattern": func(m map[string]any) {
			m["database"].(map[string]any)["major_version"] = "17.x" // pattern ^[0-9]+$
		},
	}
	for name, mutate := range cases {
		var v any
		_ = json.Unmarshal([]byte(mustSpec(t, mutate)), &v)
		if failures := ValidateCanonical(v); len(failures) == 0 {
			t.Fatalf("%s: schema-only rule not enforced", name)
		}
	}
}

// E–G handler level: 400 VALIDATION_FAILED with structured details, no
// workload, no idempotency record; the same key stays usable afterwards.
func TestInvalidRejectedWithoutState(t *testing.T) {
	resetStore()
	before := len(store.ListWorkloads())
	bad := mustSpec(t, func(m map[string]any) { delete(m, "requirements") })
	rec := postWorkload("iiiiiiiiiiiiiiii", bad) // E
	if rec.Code != 400 {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["code"] != "VALIDATION_FAILED" {
		t.Fatalf("code: %v", errObj["code"])
	}
	details, _ := errObj["details"].(map[string]any)
	failures, _ := details["failures"].([]any)
	if len(failures) == 0 {
		t.Fatal("missing structured validation details")
	}
	if n := len(store.ListWorkloads()); n != before { // F
		t.Fatalf("invalid request created a workload")
	}
	if _, _, _, found := store.CheckIdem("iiiiiiiiiiiiiiii"); found { // G
		t.Fatal("invalid request left an idempotency record")
	}
	// Same key with a valid body afterwards must succeed (no poisoning).
	rec2 := postWorkload("iiiiiiiiiiiiiiii", fullSpecBody())
	if rec2.Code != 201 {
		t.Fatalf("key poisoned by earlier invalid request: %d", rec2.Code)
	}
}

// H–J: existing semantics preserved (explicit pins; full coverage lives in
// TestRegisterReplay, TestIdemConflict, TestIdemConcurrentIdentical).
func TestValidationPreservesIdempotency(t *testing.T) {
	resetStore()
	if c, _ := register(t, "jjjjjjjjjjjjjjjj"); c != 201 {
		t.Fatalf("setup: %d", c)
	}
	c, b := register(t, "jjjjjjjjjjjjjjjj") // H: replay
	if c != 201 || b == "" {
		t.Fatalf("replay broken: %d", c)
	}
	changed := strings.Replace(fullSpecBody(), `"replicas":2`, `"replicas":3`, 1)
	req := httptest.NewRequest(http.MethodPost, "/v1/workloads", strings.NewReader(changed))
	req.Header.Set("Idempotency-Key", "jjjjjjjjjjjjjjjj")
	withBearer(req, "test-admin", "")
	rec := httptest.NewRecorder()
	registerWorkload(rec, req)
	if rec.Code != 409 { // I
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

// PG-backed path (opt-in): runs only when DATABASE_URL is set, e.g. against
// the live lab. Cleans up every row it creates.
func TestPGValidationLive(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL unset")
	}
	pg, err := NewPGStore(url)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	defer pg.Close()
	prev := store
	store = pg
	defer func() { store = prev }()
	cleanup := func(id, key string) {
		db, err := sql.Open("postgres", url)
		if err != nil {
			return
		}
		defer db.Close()
		_, _ = db.Exec(`DELETE FROM audit_events WHERE workload_id::text = $1 OR idempotency_key = $2`, id, key)
		_, _ = db.Exec(`DELETE FROM idempotency_records WHERE key = $1`, key)
		_, _ = db.Exec(`DELETE FROM workloads WHERE id::text = $1`, id)
	}

	key := "pgval-live-test-001"
	rec := postWorkload(key, fullSpecBody())
	if rec.Code != 201 {
		t.Fatalf("valid PG register: %d %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id, _ := created["id"].(string)
	defer cleanup(id, key)
	defer cleanup("", "pgval-live-test-002")

	got, ok := store.GetWorkload(id)
	if !ok {
		t.Fatal("PG workload not found after 201")
	}
	cs, _ := got["canonical_spec"].(map[string]any)
	if _, ok := cs["requirements"]; !ok {
		t.Fatalf("PG canonical_spec incomplete: %v", cs)
	}

	bad := mustSpec(t, func(m map[string]any) { delete(m, "database") })
	rec2 := postWorkload("pgval-live-test-002", bad)
	if rec2.Code != 400 {
		t.Fatalf("expected 400, got %d", rec2.Code)
	}
	var count int
	db, _ := sql.Open("postgres", url)
	defer db.Close()
	_ = db.QueryRow(`SELECT count(*) FROM workloads WHERE name = 'cloudshop' AND id::text = $1`, "00000000-0000-0000-0000-000000000000").Scan(&count)
	if _, _, _, found := store.CheckIdem("pgval-live-test-002"); found {
		t.Fatal("invalid PG request left an idempotency record")
	}
}
