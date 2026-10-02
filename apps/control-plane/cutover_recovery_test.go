// Cutover recovery matrix: every failure point between quiesce and resume
// (run: go test ./...). Each case proves no split brain, no duplicate
// transfer, no reversal, and deterministic resume.
package main

import (
	"testing"
)

// Failure before quiesce: the quiesce transition itself fails.
func TestCutoverQuiesceTransitionFailure(t *testing.T) {
	wid, migID, apprID, _, qsrc, _, restore := cutoverFixture(t, "cutoverrecq00001")
	defer restore()
	// Preflight observes quiesced, but the ensuring transition fails.
	qsrc.quiesced = true
	qsrc.failQuiesce = errFake("quiesce admin down")
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverrecqkey01", cutoverBody(apprID)))
	if out["status"] != CutoverBlocked || out["stage"] != "WRITES_QUIESCED" {
		t.Fatalf("status=%v stage=%v", out["status"], out["stage"])
	}
	if _, ok := store.GetOwnership(migID); ok {
		t.Fatal("quiesce failure must not transfer")
	}
	if qsrc.ownership != "aws" {
		t.Fatalf("source flipped without quiesce: %s", qsrc.ownership)
	}
}

// Failure during CDC catch-up: replication error blocks preflight.
func TestCutoverCDCFailure(t *testing.T) {
	wid, migID, apprID, cf, _, _, restore := cutoverFixture(t, "cutoverrecc00001")
	defer restore()
	cf.RunErr = errFake("redpanda unreachable")
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverrecckey01", cutoverBody(apprID)))
	if out["status"] != CutoverBlocked || out["stage"] != "FINAL_PREFLIGHT" {
		t.Fatalf("status=%v stage=%v", out["status"], out["stage"])
	}
	if _, ok := store.GetOwnership(migID); ok {
		t.Fatal("cdc failure must not transfer")
	}
}

// Failure after target flip: the flip is lost (non-sticky admin), verify
// refuses, both sides stay rejecting. Under commit-before-expose (H-3) the
// ownership fact was already committed, so the failed attempt leaves the
// authoritative azure record and healing the admin lets retry converge
// exactly once (previously the record was absent; the crash window is closed
// by making the fact precede exposure).
func TestCutoverVerifyFailure(t *testing.T) {
	wid, migID, apprID, _, qsrc, qtgt, restore := cutoverFixture(t, "cutoverrecv00001")
	defer restore()
	qtgt.nonSticky = true
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverrecvkey01", cutoverBody(apprID)))
	if out["status"] != CutoverBlocked || out["failure_code"] != CutoverVerifyFailed {
		t.Fatalf("status=%v failure=%v", out["status"], out["failure_code"])
	}
	// Source flipped (rejects), target lost the flip (still rejects): paused,
	// never dual-authoritative.
	if qsrc.ownership != "azure" || qtgt.ownership != "aws" {
		t.Fatalf("unsafe states src=%s tgt=%s", qsrc.ownership, qtgt.ownership)
	}
	if rec, ok := store.GetOwnership(migID); !ok || rec.CurrentOwner != "azure" {
		t.Fatalf("committed fact must stand: %+v %v", rec, ok)
	}
	// Healing the admin and retrying completes exactly once.
	qtgt.nonSticky = false
	out2 := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverrecvkey02", cutoverBody(apprID)))
	if out2["status"] != CutoverComplete {
		t.Fatalf("healed retry: status=%v failure=%v", out2["status"], out2["failure_code"])
	}
}

// Worker restart: fresh client objects (no in-memory state) resume a partial
// cutover to exactly one transfer.
func TestCutoverWorkerRestart(t *testing.T) {
	wid, migID, apprID, _, _, qtgt, restore := cutoverFixture(t, "cutoverrestart001")
	defer restore()
	qtgt.failOwnership = errFake("target admin unreachable")
	out := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverrestartkey1", cutoverBody(apprID)))
	if out["status"] != CutoverBlocked {
		t.Fatalf("partial: status=%v", out["status"])
	}
	// Simulate restart: drop every in-memory client; store persists.
	useQuiesceFakes(&fakeQuiesceClient{quiesced: true, ownership: "azure"},
		&fakeQuiesceClient{quiesced: false, ownership: "aws"})
	out2 := decodeCutover(t, cutoverFinalReq(t, wid, migID, "cutoverrestartkey2", cutoverBody(apprID)))
	if out2["status"] != CutoverComplete {
		t.Fatalf("restarted resume: status=%v failure=%v %v", out2["status"], out2["failure_code"], out2["failure_reason"])
	}
	rec, ok := store.GetOwnership(migID)
	if !ok || rec.CurrentOwner != "azure" || rec.PreviousOwner != "aws" {
		t.Fatalf("record: %+v %v", rec, ok)
	}
}

// Audit actions follow stage order (traffic switch only after transfer).
func TestCutoverAuditOrder(t *testing.T) {
	wid, migID, apprID, _, _, _, restore := cutoverFixture(t, "cutoverorder00001")
	defer restore()
	if rec := cutoverFinalReq(t, wid, migID, "cutoverorderkey01", cutoverBody(apprID)); rec.Code != 200 {
		t.Fatalf("setup: %d", rec.Code)
	}
	ms := store.(*MemStore)
	var seq []string
	for _, a := range ms.audits {
		switch a.Action {
		case "final_preflight", "final_quiesce", "transfer_ownership",
			"switch_routing", "resume_writes", "cutover_complete":
			seq = append(seq, a.Action)
		}
	}
	want := []string{"final_preflight", "final_quiesce", "transfer_ownership",
		"switch_routing", "resume_writes", "cutover_complete"}
	if len(seq) != len(want) {
		t.Fatalf("audit order: %v", seq)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("audit order: %v want %v", seq, want)
		}
	}
}
