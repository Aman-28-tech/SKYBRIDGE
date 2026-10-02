"""Structured output contract for the AI planner/validator (advisory only).

The review is JSON with a fixed shape. The model must NEVER emit an
authoritative control-plane decision: ``authorization`` is always the
literal ``NEVER_BY_AI``, and no ``decision`` field with an allow/deny
semantic may appear.
"""
PROMPT_VERSION = "ai-planner-v1"

AUTHORIZATION_NEVER = "NEVER_BY_AI"

CONFIDENCE_VALUES = ("low", "medium", "high")

# A review response is invalid if it carries one of these fields with an
# authoritative decision value. Plain discussion of approvals/policy in
# prose lists is fine; a machine-readable decision is not.
FORBIDDEN_DECISION_FIELDS = ("decision", "verdict", "authorization_decision", "approval_decision")
FORBIDDEN_DECISION_VALUES = ("allow", "allowed", "deny", "denied", "approve", "approved", "execute", "executes")

REVIEW_SCHEMA = {
    "type": "object",
    "required": [
        "migration_id",
        "assessment",
        "proposed_plan_changes",
        "confidence",
        "authorization",
    ],
    "properties": {
        "migration_id": {"type": "string"},
        "assessment": {
            "type": "object",
            "required": [
                "summary",
                "risks",
                "warnings",
                "missing_evidence",
                "recommended_checks",
                "evidence_references",
            ],
            "properties": {
                "summary": {"type": "string"},
                "risks": {
                    "type": "array",
                    "items": {
                        "type": "object",
                        "required": ["statement", "evidence_refs"],
                        "properties": {
                            "statement": {"type": "string"},
                            "evidence_refs": {"type": "array", "items": {"type": "string"}},
                        },
                    },
                },
                "warnings": {
                    "type": "array",
                    "items": {
                        "type": "object",
                        "required": ["statement", "evidence_refs"],
                        "properties": {
                            "statement": {"type": "string"},
                            "evidence_refs": {"type": "array", "items": {"type": "string"}},
                        },
                    },
                },
                "missing_evidence": {"type": "array", "items": {"type": "string"}},
                "recommended_checks": {"type": "array", "items": {"type": "string"}},
                "evidence_references": {"type": "array", "items": {"type": "string"}},
            },
        },
        "proposed_plan_changes": {"type": "array"},
        "confidence": {"type": "string", "enum": list(CONFIDENCE_VALUES)},
        "authorization": {"type": "string", "const": AUTHORIZATION_NEVER},
    },
}


def blank_review(migration_id: str) -> dict:
    return {
        "migration_id": migration_id,
        "assessment": {
            "summary": "",
            "risks": [],
            "warnings": [],
            "missing_evidence": [],
            "recommended_checks": [],
            "evidence_references": [],
        },
        "proposed_plan_changes": [],
        "confidence": "low",
        "authorization": AUTHORIZATION_NEVER,
    }
