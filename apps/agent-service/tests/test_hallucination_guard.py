"""Hallucination-guard tests: the validator rejects ungrounded responses."""
import copy
import json

from conftest import MID, make_bundle

from skybridge_ai.evidence import evidence_id_set
from skybridge_ai.provider import DeterministicMockProvider
from skybridge_ai.validate import parse_and_validate, validate_review


def valid_review(bundle):
    raw = DeterministicMockProvider().complete(
        system_prompt="t", evidence_json=json.dumps(bundle), timeout=10)
    review, ok, reason = parse_and_validate(raw, evidence_id_set(bundle))
    assert ok, reason
    return review


def test_references_must_exist(bundle):
    review = valid_review(bundle)
    known = evidence_id_set(bundle)
    for r in review["assessment"]["evidence_references"]:
        assert r in known


def test_fabricated_resource_rejected(bundle):
    review = valid_review(bundle)
    review["assessment"]["risks"].append({
        "statement": "RDS instance db-prod is undersized.",
        "evidence_refs": ["aws-rds:db-prod"],
    })
    ok, reason = validate_review(review, evidence_id_set(bundle))
    assert not ok and reason.startswith("fabricated_evidence_reference")


def test_fabricated_azure_capability_rejected(bundle):
    review = valid_review(bundle)
    review["assessment"]["warnings"].append({
        "statement": "Azure Front Door SKU mismatch.",
        "evidence_refs": ["azure-frontdoor:premium"],
    })
    ok, reason = validate_review(review, evidence_id_set(bundle))
    assert not ok and "fabricated" in reason


def test_fabricated_cdc_metric_rejected(bundle):
    review = valid_review(bundle)
    review["assessment"]["risks"].append({
        "statement": "CDC lag is 900s.",
        "evidence_refs": ["cdc_snapshot:made-up-migration"],
    })
    ok, _ = validate_review(review, evidence_id_set(bundle))
    assert not ok


def test_fabricated_approval_rejected(bundle):
    review = valid_review(bundle)
    review["assessment"]["evidence_references"].append("approval:00000000-0000-4000-8000-000000000000")
    ok, reason = validate_review(review, evidence_id_set(bundle))
    assert not ok and "fabricated" in reason


def test_fabricated_policy_decision_field_rejected(bundle):
    review = valid_review(bundle)
    review["decision"] = "allow"
    ok, reason = validate_review(review, evidence_id_set(bundle))
    assert not ok and "forbidden_authoritative_decision" in reason


def test_approve_decision_rejected(bundle):
    review = valid_review(bundle)
    review["assessment"]["verdict"] = "APPROVED"
    ok, reason = validate_review(review, evidence_id_set(bundle))
    assert not ok and "forbidden_authoritative_decision" in reason


def test_wrong_authorization_rejected(bundle):
    review = valid_review(bundle)
    review["authorization"] = "ALLOW"
    ok, reason = validate_review(review, evidence_id_set(bundle))
    assert not ok and reason.startswith("invalid_authorization")


def test_missing_evidence_reference_rejected(bundle):
    review = valid_review(bundle)
    review["assessment"]["risks"].append({"statement": "Vague risk.", "evidence_refs": []})
    ok, reason = validate_review(review, evidence_id_set(bundle))
    assert not ok and reason.startswith("missing_evidence_reference")


def test_invalid_enum_rejected(bundle):
    review = valid_review(bundle)
    review["confidence"] = "certain"
    ok, reason = validate_review(review, evidence_id_set(bundle))
    assert not ok and reason.startswith("invalid_enum")


def test_malformed_json_rejected(bundle):
    review, ok, reason = parse_and_validate("{not json", evidence_id_set(bundle))
    assert review is None and not ok and reason.startswith("malformed_json")


def test_non_object_rejected(bundle):
    review, ok, reason = parse_and_validate("[1,2]", evidence_id_set(bundle))
    assert review is None and not ok


def test_missing_field_rejected(bundle):
    review = valid_review(bundle)
    del review["assessment"]["summary"]
    ok, reason = validate_review(review, evidence_id_set(bundle))
    assert not ok and "missing" in reason


def test_migration_id_mismatch_rejected_by_orchestrator(bundle):
    import skybridge_ai.review as R
    review = valid_review(bundle)
    review["migration_id"] = "other-migration"
    monkey = {"called": False}

    class FakeProvider:
        model_id = "fake"

        def complete(self, *, system_prompt, evidence_json, timeout):
            return json.dumps(review)

    orig = R.fetch_evidence
    R.fetch_evidence = lambda *a, **k: bundle
    try:
        status, envelope = R.build_review_envelope(
            workload_id=bundle["workload_id"], migration_id=MID,
            target_provider="azure", base_url="http://x", provider=FakeProvider())
    finally:
        R.fetch_evidence = orig
    assert status == 502 and envelope["review"] is None
    assert "mismatch" in (envelope["error"] or "")
