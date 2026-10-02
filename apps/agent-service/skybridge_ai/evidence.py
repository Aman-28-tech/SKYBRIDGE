"""Authoritative evidence retrieval for the AI reviewer.

Every fact the model sees comes from SKYBRIDGE control-plane GET
endpoints (never POST/PUT/PATCH/DELETE, never direct database access,
never cloud APIs). Missing evidence stays missing: the bundle records
``None`` and the review reports it under ``missing_evidence`` instead of
inventing values.
"""
import hashlib
import json
import urllib.error
import urllib.parse
import urllib.request

from . import sanitize as _san

KNOWN_LIMITATIONS = [
    "Local demo environment only: PostgreSQL + Debezium + Redpanda + control plane on one machine.",
    "Real AWS status: DISABLED / BLOCKED. No credentials loaded, no calls made, no spending.",
    "Real Azure status: DISABLED / BLOCKED. Untouched, zero calls made.",
    "Terraform Apply status: DISABLED. Modules are fmt/validate only; apply never runs.",
    "AI output is advisory only and never authorizes or executes migration steps.",
    "Reconciliation is probe-scoped by design (seed divergence excluded).",
]


class EvidenceError(Exception):
    def __init__(self, message: str, *, status: int = 502, kind: str = "evidence_error"):
        super().__init__(message)
        self.status = status
        self.kind = kind


def _get(base_url: str, path: str, timeout: float):
    url = base_url.rstrip("/") + path
    req = urllib.request.Request(url, method="GET", headers={"Accept": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read().decode("utf-8")), resp.status
    except urllib.error.HTTPError as e:
        try:
            body = e.read().decode("utf-8")
            detail = json.loads(body) if body else {}
        except Exception:
            detail = {}
        err = detail.get("error", {}) if isinstance(detail, dict) else {}
        raise EvidenceError(
            f"GET {path}: {e.code} {err.get('code', '')} {err.get('message', '')}".strip(),
            status=e.code,
            kind="not_found" if e.code == 404 else "evidence_error",
        )
    except Exception as e:
        raise EvidenceError(f"control plane unreachable at {base_url}: {e}", status=502, kind="unreachable")


def canonical_fingerprint(bundle: dict) -> str:
    canon = json.dumps(bundle, sort_keys=True, separators=(",", ":"), default=str)
    return "sha256:" + hashlib.sha256(canon.encode("utf-8")).hexdigest()


def fetch_evidence(base_url: str, workload_id: str, migration_id: str,
                   target_provider: str, timeout: float = 10.0) -> dict:
    """Fetch and canonicalize the authoritative evidence bundle.

    Raises EvidenceError on unreachable control plane or unknown
    workload/migration. Individual optional evidence (compat, plan, drift,
    approvals, canary, cutover status) may be absent and is recorded as
    ``None`` with an explanatory missing-evidence entry.
    """
    base_url = base_url.rstrip("/")
    missing: list = []

    workload, _ = _get(base_url, f"/v1/workloads/{urllib.parse.quote(workload_id)}", timeout)
    migration, _ = _get(base_url, f"/v1/migrations/{urllib.parse.quote(migration_id)}", timeout)
    if migration.get("workload_id") not in (workload_id, None, "") and migration.get("workload_id") != workload_id:
        raise EvidenceError(
            f"migration {migration_id} does not belong to workload {workload_id}",
            status=404, kind="not_found",
        )

    def optional(path: str, label: str):
        try:
            data, _ = _get(base_url, path, timeout)
            return data
        except EvidenceError as e:
            if e.status == 404:
                missing.append(label)
                return None
            raise

    compat = optional(f"/v1/workloads/{urllib.parse.quote(workload_id)}/compatibility", "compatibility")
    plan = optional(f"/v1/workloads/{urllib.parse.quote(workload_id)}/plan", "plan")
    drift_list = optional(f"/v1/workloads/{urllib.parse.quote(workload_id)}/drift", "drift")
    approvals_list = optional(f"/v1/workloads/{urllib.parse.quote(workload_id)}/approvals", "approvals")
    canary = optional(f"/v1/migrations/{urllib.parse.quote(migration_id)}/canary", "canary")
    cutover_status = optional(f"/v1/migrations/{urllib.parse.quote(migration_id)}/cutover", "cutover_status")
    summary = optional(f"/v1/migrations/{urllib.parse.quote(migration_id)}/summary", "summary")
    audit = optional(f"/v1/migrations/{urllib.parse.quote(migration_id)}/audit", "audit")

    drift = None
    if isinstance(drift_list, dict):
        items = drift_list.get("items") or []
        drift = items[0] if items else None
    elif drift_list is None:
        pass
    approvals = []
    if isinstance(approvals_list, dict):
        approvals = (approvals_list.get("items") or [])[:20]

    audit_tail = []
    if isinstance(audit, dict):
        for entry in (audit.get("items") or [])[-25:]:
            audit_tail.append({
                "action": entry.get("action"),
                "result": entry.get("result"),
                "created_at": entry.get("created_at"),
                "request_id": entry.get("request_id"),
                "actor_id": entry.get("actor_id"),
                "policy_decision": entry.get("policy_decision"),
                "approval_id": entry.get("approval_id"),
            })

    bundle = {
        "workload_id": workload_id,
        "migration_id": migration_id,
        "target_provider": target_provider,
        "workload": workload,
        "migration": migration,
        "compatibility": compat,
        "plan": plan,
        "drift": drift,
        "approvals": approvals,
        "canary": canary,
        "cutover_status": cutover_status,
        "summary": summary,
        "audit_tail": audit_tail,
        "missing": missing,
        "known_limitations": list(KNOWN_LIMITATIONS),
    }
    # Secrets never reach the model: sanitize the whole bundle up front.
    bundle = _san.sanitize(bundle)
    bundle["evidence_fingerprint"] = canonical_fingerprint(
        {k: v for k, v in bundle.items() if k != "evidence_fingerprint"}
    )
    return bundle


def evidence_references(bundle: dict) -> list:
    """Stable evidence items {id, source, version, timestamp} for the bundle."""
    wid, mid = bundle["workload_id"], bundle["migration_id"]
    refs = [
        {"id": f"workload:{wid}", "source": f"control-plane GET /v1/workloads/{wid}",
         "version": str((bundle.get("workload") or {}).get("schema_version", "?")), "timestamp": None},
        {"id": f"migration:{mid}", "source": f"control-plane GET /v1/migrations/{mid}",
         "version": str((bundle.get("migration") or {}).get("status", "?")), "timestamp": None},
    ]
    compat = bundle.get("compatibility") or {}
    if compat.get("id"):
        refs.append({"id": f"compat:{compat['id']}",
                     "source": f"control-plane GET /v1/workloads/{wid}/compatibility",
                     "version": str(compat.get("evaluator_version", "?")), "timestamp": None})
    plan = bundle.get("plan") or {}
    if plan.get("id"):
        refs.append({"id": f"plan:{plan['id']}",
                     "source": f"control-plane GET /v1/workloads/{wid}/plan",
                     "version": str(plan.get("planner_version", "?")), "timestamp": None})
    drift = bundle.get("drift") or {}
    if drift.get("id"):
        refs.append({"id": f"drift:{drift['id']}",
                     "source": f"control-plane GET /v1/workloads/{wid}/drift",
                     "version": str(drift.get("evaluator_version", "?")), "timestamp": None})
    for appr in bundle.get("approvals") or []:
        if isinstance(appr, dict) and appr.get("id"):
            refs.append({"id": f"approval:{appr['id']}",
                         "source": f"control-plane GET /v1/workloads/{wid}/approvals",
                         "version": str(appr.get("policy_bundle_version", "?")),
                         "timestamp": appr.get("decided_at")})
    canary = bundle.get("canary") or {}
    for rec in (canary.get("items") or [])[:50]:
        if isinstance(rec, dict) and rec.get("id"):
            refs.append({"id": f"canary:{rec['id']}",
                         "source": f"control-plane GET /v1/migrations/{mid}/canary",
                         "version": f"stage-{rec.get('stage', '?')}",
                         "timestamp": rec.get("created_at")})
    summary = bundle.get("summary") or {}
    cdc = (summary.get("cdc") or {}) if isinstance(summary, dict) else {}
    if cdc:
        refs.append({"id": f"cdc_snapshot:{mid}",
                     "source": f"control-plane GET /v1/migrations/{mid}/summary (cdc view)",
                     "version": str(cdc.get("applied_lsn") or "unknown"),
                     "timestamp": None})
    ownership = (summary.get("ownership") or {}) if isinstance(summary, dict) else {}
    if ownership:
        refs.append({"id": f"ownership:{mid}",
                     "source": f"control-plane GET /v1/migrations/{mid}/summary (ownership view)",
                     "version": str(ownership.get("current_owner") or "unknown"),
                     "timestamp": None})
    for entry in bundle.get("audit_tail") or []:
        if entry.get("request_id"):
            refs.append({"id": f"audit:{entry['request_id']}",
                         "source": f"control-plane GET /v1/migrations/{mid}/audit",
                         "version": str(entry.get("action") or "?"),
                         "timestamp": entry.get("created_at")})
    return refs


def evidence_id_set(bundle: dict) -> set:
    return {r["id"] for r in evidence_references(bundle)}
