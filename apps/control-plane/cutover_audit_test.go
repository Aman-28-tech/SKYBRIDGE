// Cutover state-machine audit + ownership CAS legality (run: go test ./...).
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every transition of a completed cutover is audited with the §2 record:
// migration/run IDs, actor, request, idempotency key, ownership, positions,
// lag, policy hash, approval, timestamp. No credentials anywhere.
func TestCutoverStateMachineAudit(t *testing.T) {
	wid, migID, apprID, _, _, _, restore := cutoverFixture(t, "cutoveraudit00001")
	defer restore()
	key := "cutoverauditkey01"
	if rec := cutoverFinalReq(t, wid, migID, key, cutoverBody(apprID)); rec.Code != 200 {
		t.Fatalf("setup: %d", rec.Code)
	}
	ms := store.(*MemStore)
	byAction := map[string][]AuditEntry{}
	for _, a := range ms.audits {
		byAction[a.Action] = append(byAction[a.Action], a)
	}
	for _, want := range []string{"final_preflight", "final_quiesce", "transfer_ownership",
		"switch_routing", "resume_writes", "cutover_complete", "final_cutover"} {
		rows, ok := byAction[want]
		if !ok || len(rows) != 1 {
			t.Fatalf("audit %q: %d rows", want, len(rows))
		}
		a := rows[0]
		if a.RunID != migID || a.WorkloadID != wid {
			t.Fatalf("%s: ids %+v", want, a)
		}
		if a.RequestID == "" || a.ActorID == "" || a.ActorType == "" {
			t.Fatalf("%s: identity %+v", want, a)
		}
		if a.PolicyBundleVersion == "" {
			t.Fatalf("%s: bundle %+v", want, a)
		}
		var meta map[string]any
		if err := json.Unmarshal([]byte(a.Metadata), &meta); err != nil {
			t.Fatalf("%s: metadata not JSON: %v", want, err)
		}
		if _, ok := meta["at"]; !ok {
			t.Fatalf("%s: timestamp missing", want)
		}
		for _, v := range []string{a.Metadata} {
			l := strings.ToLower(v)
			if strings.Contains(l, "password") || strings.Contains(l, "secret") || strings.Contains(l, "token") {
				t.Fatalf("%s: secret leakage", want)
			}
		}
	}
	// Transfer record carries ownership + positions + lag.
	tr := byAction["transfer_ownership"][0]
	for _, k := range []string{"previous_owner", "current_owner", "source_lsn", "applied_lsn", "cdc_lag_seconds"} {
		var meta map[string]any
		_ = json.Unmarshal([]byte(tr.Metadata), &meta)
		if _, ok := meta[k]; !ok {
			t.Fatalf("transfer metadata lacks %q: %s", k, tr.Metadata)
		}
	}
	if tr.ApprovalID != apprID {
		t.Fatalf("transfer approval: %+v", tr)
	}
	// Idempotency keys recorded on mutating boundaries.
	if byAction["final_cutover"][0].IdempotencyKey != key {
		t.Fatalf("idem key: %+v", byAction["final_cutover"][0])
	}
}

// Illegal ownership transitions are refused deterministically.
func TestOwnershipIllegalTransitions(t *testing.T) {
	resetStore()
	mk := func(owner string) OwnershipRecord {
		return OwnershipRecord{MigrationID: "m1", WorkloadID: "w1",
			CurrentOwner: owner, PreviousOwner: "aws", Routing: owner}
	}
	// Absent record: only expect-aws succeeds.
	if ok, _ := store.TransferOwnership("m1", "azure", mk("azure")); ok {
		t.Fatal("expect-azure on absent record must fail")
	}
	if _, ok := store.GetOwnership("m1"); ok {
		t.Fatal("failed transfer must not create a record")
	}
	// Happy path: absent + expect aws -> azure.
	if ok, _ := store.TransferOwnership("m1", "aws", mk("azure")); !ok {
		t.Fatal("aws->azure must succeed")
	}
	// No second transfer, no reversal, no re-commit.
	if ok, _ := store.TransferOwnership("m1", "aws", mk("azure")); ok {
		t.Fatal("second transfer must fail")
	}
	if ok, _ := store.TransferOwnership("m1", "azure", mk("aws")); ok {
		t.Fatal("reversal azure->aws must fail")
	}
	if ok, _ := store.TransferOwnership("m1", "azure", mk("azure")); ok {
		t.Fatal("re-commit over azure must fail")
	}
	if ok, _ := store.TransferOwnership("m1", "aws", mk("aws")); ok {
		t.Fatal("downgrade record must fail")
	}
	rec, ok := store.GetOwnership("m1")
	if !ok || rec.CurrentOwner != "azure" || rec.PreviousOwner != "aws" {
		t.Fatalf("record corrupted: %+v", rec)
	}
	if _, ok := store.GetOwnership("nope"); ok {
		t.Fatal("absent lookup must miss")
	}
}
