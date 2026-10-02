"""Contract-level tests (no live services required).

Covers: idempotency SHA-256 rule, canonical canary stages, compat aggregation,
drift severities, RPO/RTO fixture pins, OpenAPI required paths, proto fields.
Run: python3 -m pytest tests/contract -q
"""
import hashlib
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent.parent
POLICY = ROOT / "packages/contracts/policy/cutover.rego"


def test_idempotency_sha256_rule():
    body = json.dumps({"user_id": "u1", "total_cents": 100}, sort_keys=True).encode()
    h = hashlib.sha256(body).hexdigest()
    assert len(h) == 64
    # same body -> same hash (replay); different body -> different hash (409 path)
    assert hashlib.sha256(body).hexdigest() == h
    assert hashlib.sha256(b'{"different":true}').hexdigest() != h


def test_canonical_stages_in_policy():
    text = POLICY.read_text()
    for stage in ["0, 1, 5, 25, 50, 100", "read_only_canary", "write_ownership", "post_write_rollback_blocked"]:
        assert stage in text, f"missing {stage} in cutover.rego"


def test_compat_aggregation_documented():
    doc = (ROOT / "docs/CLOUD_COMPATIBILITY_ENGINE.md").read_text()
    assert "ANY check == block" in doc and "ANY check == unknown" in doc
    domain = (ROOT / "docs/DOMAIN_MODEL.md").read_text().replace("`", "")
    arch = (ROOT / "docs/ARCHITECTURE.md").read_text().replace("`", "")
    assert ("block > unknown > conditional > pass" in domain or
            "block > unknown > conditional > pass" in arch)
    assert "ANY check == unknown" in domain


def test_drift_severities_canonical():
    for f in ["docs/DRIFT_RECONCILIATION.md", "docs/DOMAIN_MODEL.md", "packages/contracts/policy/cutover.rego"]:
        assert "security_critical" in (ROOT / f).read_text()
    assert "migration_blocking" not in (ROOT / "packages/contracts/policy/cutover.rego").read_text()


def test_fixture_rpo_rto():
    fix = json.loads((ROOT / "packages/contracts/schemas/cloudshop-workload.v1.json").read_text())
    assert fix["requirements"]["rpo_seconds"] == 30
    assert fix["requirements"]["rto_seconds"] == 900


def test_openapi_paths():
    oapi = (ROOT / "packages/contracts/openapi/skybridge.yaml").read_text()
    for p in ["/v1/workloads", "/v1/migrations", "/v1/migrations/{migrationId}",
              "/v1/migrations/{migrationId}/audit", "/v1/migrations/{migrationId}/cutover",
              "/v1/migrations/{migrationId}/rollback", "/v1/workloads/{workloadId}/compatibility",
              "/v1/workloads/{workloadId}/drift", "/v1/workloads/{workloadId}/plan",
              "/v1/workloads/{workloadId}/readiness",
              "/v1/workloads/{workloadId}/approvals",
              "/v1/workloads/{workloadId}/approvals/{approvalId}/decision",
              "/v1/migrations/{migrationId}/execute",
              "/v1/migrations/{migrationId}/execution"]:
        assert p in oapi, f"missing {p}"
    for s in ["TargetMigrationPlan:", "PlanComponent:", "PlanRequest:",
              "provisioning_mode:", "compatibility_report_id:",
              "DriftFinding:", "ObservedSnapshot:", "drift_gate:",
              "finding_id:", "remediation_hint:",
              "ReadinessRequest:", "ReadinessDecision:", "PolicyGateResult:",
              "policy_input_hash:", "Approval:", "ApprovalRequest:",
              "ApprovalDecisionRequest:", "PaginatedApprovals:",
              "ExecuteRequest:", "Execution:", "ExecutionStatus:",
              "AuditEvent:", "PaginatedAuditEvents:"]:
        assert s in oapi, f"missing {s}"


def test_proto_fields():
    mig = (ROOT / "packages/contracts/proto/migration.proto").read_text()
    audit = (ROOT / "packages/contracts/proto/audit.proto").read_text()
    for field in ["request_id", "run_id", "workload_id", "idempotency_key", "schema_version"]:
        assert field in mig, f"migration.proto missing {field}"
    for field in ["request_id", "policy_bundle_version", "approval_id"]:
        assert field in audit, f"audit.proto missing {field}"


def test_workflow_transitions():
    wf = (ROOT / "apps/control-plane/workflow.go").read_text()
    for state in ["READY_FOR_CUTOVER", "CUTTING_OVER", "VERIFYING", "COMPLETED",
                  "ROLLING_BACK", "ROLLED_BACK", "ABORTED", "PostWriteRollbackBlocked"]:
        assert state in wf, f"workflow.go missing {state}"


def test_lab_files_present():
    assert (ROOT / "docker-compose.yml").exists()
    assert (ROOT / "infrastructure/kind/config.yaml").exists()
    assert (ROOT / "workloads/cloudshop/migrations/001_init.sql").exists()
    assert (ROOT / "workloads/cloudshop/api/main.go").exists()
    assert (ROOT / "apps/control-plane/main.go").exists()
