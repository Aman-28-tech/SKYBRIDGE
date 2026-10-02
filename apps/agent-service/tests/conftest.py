"""Shared fixtures for AI reviewer tests: synthetic evidence bundles.

Bundles mirror the shape produced by fetch_evidence() but need no live
control plane. Deterministic IDs keep reproducibility tests meaningful.
"""
import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import pytest  # noqa: E402

from skybridge_ai.evidence import canonical_fingerprint  # noqa: E402

WID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaa0001"
MID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
COMPAT_ID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
PLAN_ID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
DRIFT_ID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
APPR_ID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
CANARY_ID = "11111111-1111-4111-8111-111111111111"


def _stage(name, status):
    return {"name": name, "status": status, "timestamp": "", "detail": ""}


def make_bundle(**overrides):
    bundle = {
        "workload_id": WID,
        "migration_id": MID,
        "target_provider": "azure",
        "workload": {
            "id": WID, "name": "cloudshop", "schema_version": 1,
            "lifecycle_state": "registered",
            "canonical_spec": {"requirements": {"rpo_seconds": 30}},
        },
        "migration": {"id": MID, "workload_id": WID, "status": "REGISTERED",
                      "current_step": "registered"},
        "compatibility": {
            "id": COMPAT_ID, "workload_id": WID, "target_provider": "azure",
            "status": "pass", "checks": [], "evaluator_version": "compat-v1",
        },
        "plan": {
            "id": PLAN_ID, "workload_id": WID, "target_provider": "azure",
            "overall_status": "pass", "components": [], "blockers": [],
            "planner_version": "planner-v2",
        },
        "drift": {
            "id": DRIFT_ID, "workload_id": WID, "severity": "informational",
            "status": "open", "drift_gate": "clear", "findings": [],
            "evaluator_version": "drift-v1",
        },
        "approvals": [],
        "canary": {"items": [], "latest": {}, "expected_stage": 0},
        "cutover_status": None,
        "summary": {
            "lifecycle": {"current": "COMPATIBILITY", "stages": [
                _stage("REGISTERED", "completed"),
                _stage("COMPATIBILITY", "completed"),
                _stage("PLANNED", "current"),
                _stage("REHEARSED", "pending"),
                _stage("CANARY", "pending"),
                _stage("QUIESCED", "pending"),
                _stage("CUTOVER", "pending"),
                _stage("COMPLETE", "pending"),
            ]},
            "cdc": {"source_lsn": "0/A001", "applied_lsn": "0/A001",
                    "lag_seconds": 2, "rpo_seconds": 30,
                    "events_captured": 7, "events_applied": 7,
                    "events_duplicates": 0, "reconciliation_match": True,
                    "within_rpo": True},
            "ownership": {"current_owner": "aws", "routing": "aws",
                          "source_writable": True, "target_writable": False,
                          "split_brain": "SAFE"},
            "safety": {"compatibility": "pass", "drift_gate": "clear",
                       "policy_decision": "allow", "quiesce": "accepting",
                       "rollback": "PRE_WRITE_AVAILABLE", "split_brain": "SAFE"},
        },
        "audit_tail": [{"action": "create_migration", "result": "success",
                        "created_at": "2026-01-01T00:00:00Z",
                        "request_id": "req_abc123", "actor_id": "operator",
                        "policy_decision": "allow", "approval_id": ""}],
        "missing": [],
        "known_limitations": ["Local demo only."],
    }
    bundle.update(overrides)
    bundle["evidence_fingerprint"] = canonical_fingerprint(
        {k: v for k, v in bundle.items() if k != "evidence_fingerprint"})
    return bundle


@pytest.fixture
def bundle():
    return make_bundle()
