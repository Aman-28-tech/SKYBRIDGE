"""Fail-safe validation of AI review responses.

A response is accepted only if ALL of these hold:
  1. well-formed JSON object matching the review schema (required fields,
     string arrays, confidence enum);
  2. ``authorization`` is exactly ``NEVER_BY_AI``;
  3. no ``decision``-family field carries an authoritative value
     (allow/deny/approve/execute);
  4. every risk and warning cites >=1 evidence id, and every cited id
     (risks, warnings, top-level references) exists in the supplied
     evidence set -- fabricated references are rejected;
  5. no secret material leaked into the response.

Anything else -> ``(False, reason)`` and the service returns a fail-safe
envelope with ``review: null`` instead of a fabricated review.
"""
from . import sanitize as _san
from .schema import (
    AUTHORIZATION_NEVER,
    CONFIDENCE_VALUES,
    FORBIDDEN_DECISION_FIELDS,
    FORBIDDEN_DECISION_VALUES,
)


def _is_str_list(v) -> bool:
    return isinstance(v, list) and all(isinstance(x, str) for x in v)


def validate_review(review, evidence_ids: set) -> tuple:
    """Return (ok, reason). ``reason`` is 'valid' when ok."""
    if not isinstance(review, dict):
        return False, "invalid_schema: review is not a JSON object"
    for field in ("migration_id", "assessment", "proposed_plan_changes", "confidence", "authorization"):
        if field not in review:
            return False, f"invalid_schema: missing field {field!r}"
    if not isinstance(review["migration_id"], str) or not review["migration_id"]:
        return False, "invalid_schema: migration_id must be a non-empty string"
    if review["authorization"] != AUTHORIZATION_NEVER:
        return False, f"invalid_authorization: must be {AUTHORIZATION_NEVER!r}"
    if review["confidence"] not in CONFIDENCE_VALUES:
        return False, f"invalid_enum: confidence must be one of {CONFIDENCE_VALUES}"
    if not isinstance(review["proposed_plan_changes"], list):
        return False, "invalid_schema: proposed_plan_changes must be an array"

    assessment = review["assessment"]
    if not isinstance(assessment, dict):
        return False, "invalid_schema: assessment must be an object"
    for field in ("summary", "risks", "warnings", "missing_evidence",
                  "recommended_checks", "evidence_references"):
        if field not in assessment:
            return False, f"invalid_schema: assessment missing {field!r}"
    if not isinstance(assessment["summary"], str) or not assessment["summary"].strip():
        return False, "invalid_schema: summary must be a non-empty string"
    for field in ("missing_evidence", "recommended_checks", "evidence_references"):
        if not _is_str_list(assessment[field]):
            return False, f"invalid_schema: {field} must be an array of strings"
    for group in ("risks", "warnings"):
        items = assessment[group]
        if not isinstance(items, list):
            return False, f"invalid_schema: {group} must be an array"
        for i, item in enumerate(items):
            if not isinstance(item, dict):
                return False, f"invalid_schema: {group}[{i}] must be an object"
            if not isinstance(item.get("statement"), str) or not item["statement"].strip():
                return False, f"invalid_schema: {group}[{i}].statement must be non-empty"
            refs = item.get("evidence_refs")
            if not isinstance(refs, list) or not refs or not all(isinstance(r, str) for r in refs):
                return False, f"missing_evidence_reference: {group}[{i}] cites no evidence"
            for r in refs:
                if r not in evidence_ids:
                    return False, f"fabricated_evidence_reference: {r!r} in {group}[{i}]"

    for r in assessment["evidence_references"]:
        if r not in evidence_ids:
            return False, f"fabricated_evidence_reference: {r!r} in evidence_references"

    for field in FORBIDDEN_DECISION_FIELDS:
        for holder in (review, assessment):
            if field in holder:
                val = holder[field]
                if isinstance(val, str) and val.strip().lower() in FORBIDDEN_DECISION_VALUES:
                    return False, f"forbidden_authoritative_decision: {field}={val!r}"
                if field == "decision" and isinstance(val, str) and val.strip():
                    # Any free-form machine-readable decision is out of contract.
                    return False, f"forbidden_authoritative_decision: unexpected decision field {val!r}"

    for change in review["proposed_plan_changes"]:
        if isinstance(change, dict):
            for r in change.get("evidence_refs", []) or []:
                if r not in evidence_ids:
                    return False, f"fabricated_evidence_reference: {r!r} in proposed_plan_changes"

    text = _flatten_strings(review)
    for marker in ("AKIA", "xoxb-", "ghp_", "-----BEGIN"):
        if marker in text:
            return False, "secret_leak: response contains secret-like material"
    return True, "valid"


def _flatten_strings(node) -> str:
    if isinstance(node, str):
        return node
    if isinstance(node, dict):
        return " ".join(_flatten_strings(v) for v in node.values())
    if isinstance(node, list):
        return " ".join(_flatten_strings(v) for v in node)
    return ""


def parse_and_validate(raw: str, evidence_ids: set) -> tuple:
    """Parse provider output and validate. Returns (review_or_None, ok, reason)."""
    try:
        import json as _json
        review = _json.loads(raw)
    except Exception as e:
        return None, False, f"malformed_json: {e}"
    ok, reason = validate_review(review, evidence_ids)
    if not ok:
        return None, False, reason
    return review, True, "valid"
