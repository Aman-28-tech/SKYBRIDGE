"""Valid-review and evidence-scenario tests for the deterministic provider."""
from conftest import APPR_ID, CANARY_ID, MID, WID, make_bundle

from skybridge_ai.evidence import evidence_id_set
from skybridge_ai.provider import DeterministicMockProvider
from skybridge_ai.validate import validate_review


def run(bundle):
    raw = DeterministicMockProvider().complete(
        system_prompt="test", evidence_json=__import__("json").dumps(bundle), timeout=10)
    import json
    review = json.loads(raw)
    ok, reason = validate_review(review, evidence_id_set(bundle))
    assert ok, reason
    return review


def test_valid_review_clean_evidence(bundle):
    review = run(bundle)
    assert review["migration_id"] == MID
    assert review["authorization"] == "NEVER_BY_AI"
    assert review["confidence"] in ("low", "medium", "high")
    assert review["assessment"]["summary"]
    # Clean evidence: no risks expected, rehearsal/canary checks recommended.
    assert review["assessment"]["risks"] == []
    assert any("rehearsal" in c.lower() for c in review["assessment"]["recommended_checks"])


def test_missing_evidence_reported_not_invented():
    bundle = make_bundle(compatibility=None, plan=None, drift=None)
    review = run(bundle)
    assert any("compatibility" in m for m in review["assessment"]["missing_evidence"])
    assert any("migration plan" in m for m in review["assessment"]["missing_evidence"])
    assert any("drift" in m for m in review["assessment"]["missing_evidence"])


def test_compatibility_block_is_risk():
    bundle = make_bundle()
    bundle["compatibility"]["status"] = "block"
    review = run(bundle)
    assert any("block" in r["statement"] for r in review["assessment"]["risks"])
    assert review["confidence"] == "low"


def test_conditional_compatibility_is_warning():
    bundle = make_bundle()
    bundle["compatibility"]["status"] = "conditional"
    review = run(bundle)
    assert review["assessment"]["risks"] == []
    assert any("conditional" in w["statement"] for w in review["assessment"]["warnings"])


def test_blocking_drift_is_risk():
    bundle = make_bundle()
    bundle["drift"]["severity"] = "blocking"
    review = run(bundle)
    assert any("Drift" in r["statement"] for r in review["assessment"]["risks"])


def test_security_critical_drift_is_risk():
    bundle = make_bundle()
    bundle["drift"]["severity"] = "security_critical"
    review = run(bundle)
    assert any("security_critical" in r["statement"] for r in review["assessment"]["risks"])


def test_approval_pending_is_warning():
    bundle = make_bundle()
    bundle["approvals"] = [{"id": APPR_ID, "decision": "pending",
                            "policy_bundle_version": "dev"}]
    bundle["summary"]["safety"]["policy_decision"] = "approval_required"
    review = run(bundle)
    assert any("pending" in w["statement"] for w in review["assessment"]["warnings"])
    assert any("approval" in m for m in review["assessment"]["missing_evidence"])


def test_successful_rehearsal_removes_rehearsal_check():
    bundle = make_bundle()
    for stage in bundle["summary"]["lifecycle"]["stages"]:
        if stage["name"] == "REHEARSED":
            stage["status"] = "completed"
    review = run(bundle)
    assert not any("rehearsal" in c.lower() for c in review["assessment"]["recommended_checks"])


def test_canary_fail_is_risk():
    bundle = make_bundle()
    bundle["canary"] = {"items": [{"id": CANARY_ID, "stage": 5, "verdict": "FAIL"}],
                        "latest": {}, "expected_stage": 5}
    review = run(bundle)
    assert any("canary" in r["statement"].lower() for r in review["assessment"]["risks"])
    assert any(ref == f"canary:{CANARY_ID}" for r in review["assessment"]["risks"]
               for ref in r["evidence_refs"])


def test_cdc_lag_near_rpo_is_warning():
    bundle = make_bundle()
    bundle["summary"]["cdc"]["lag_seconds"] = 25  # >= 0.8 * 30
    review = run(bundle)
    assert any("close to the RPO" in w["statement"] for w in review["assessment"]["warnings"])


def test_cdc_lag_over_rpo_is_risk():
    bundle = make_bundle()
    bundle["summary"]["cdc"]["lag_seconds"] = 45
    review = run(bundle)
    assert any("exceeds the RPO" in r["statement"] for r in review["assessment"]["risks"])


def test_reconciliation_mismatch_is_risk():
    bundle = make_bundle()
    bundle["summary"]["cdc"]["reconciliation_match"] = False
    review = run(bundle)
    assert any("mismatch" in r["statement"].lower() for r in review["assessment"]["risks"])


def test_post_authority_warns_rollback_blocked():
    bundle = make_bundle()
    bundle["summary"]["ownership"]["current_owner"] = "azure"
    review = run(bundle)
    assert any("forward-fix" in w["statement"] for w in review["assessment"]["warnings"])


def test_reproducible_same_snapshot_same_review(bundle):
    first = run(bundle)
    second = run(make_bundle())
    assert first["assessment"]["risks"] == second["assessment"]["risks"]
    assert first["assessment"]["warnings"] == second["assessment"]["warnings"]
    assert first["confidence"] == second["confidence"]
