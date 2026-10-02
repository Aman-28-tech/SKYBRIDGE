"""Model providers for the AI reviewer.

``DeterministicMockProvider`` is the default and the only provider used
by tests and acceptance: it derives a structured, evidence-referencing
review from the evidence bundle with fixed rules (no network, no keys,
no randomness). ``HttpProvider`` is an optional generic
OpenAI-compatible chat-completions backend for operator experiments;
it is never required and never exercised by CI/acceptance.
"""
import concurrent.futures
import json
import time
import urllib.request

from . import sanitize as _san
from .evidence import evidence_references
from .schema import AUTHORIZATION_NEVER, blank_review


class ProviderError(Exception):
    def __init__(self, message: str, *, kind: str = "provider_error"):
        super().__init__(message)
        self.kind = kind


class DeterministicMockProvider:
    """Rule-based deterministic stand-in for an LLM.

    Produces schema-valid reviews whose every risk/warning cites a real
    evidence ID from the bundle. Same evidence snapshot -> same
    statements (modulo no timestamps/randomness by construction).
    """

    model_id = "mock-deterministic-v1"

    def complete(self, *, system_prompt: str, evidence_json: str, timeout: float) -> str:
        bundle = json.loads(evidence_json)
        return json.dumps(_mock_review(bundle))


def _lifecycle_status(summary, name: str):
    try:
        for stage in summary.get("lifecycle", {}).get("stages", []):
            if stage.get("name") == name:
                return stage.get("status")
    except AttributeError:
        pass
    return None


def _mock_review(bundle: dict) -> dict:
    mid = bundle["migration_id"]
    review = blank_review(mid)
    a = review["assessment"]
    refs = evidence_references(bundle)
    ref_ids = [r["id"] for r in refs]

    def has(prefix: str):
        return [i for i in ref_ids if i.startswith(prefix)]

    compat = bundle.get("compatibility") or {}
    plan = bundle.get("plan") or {}
    drift = bundle.get("drift") or {}
    approvals = bundle.get("approvals") or []
    canary = bundle.get("canary") or {}
    summary = bundle.get("summary") or {}
    cdc = (summary.get("cdc") or {}) if isinstance(summary, dict) else {}
    ownership = (summary.get("ownership") or {}) if isinstance(summary, dict) else {}
    safety = (summary.get("safety") or {}) if isinstance(summary, dict) else {}

    compat_refs = has("compat:") or [f"migration:{mid}"]
    plan_refs = has("plan:") or [f"migration:{mid}"]
    drift_refs = has("drift:") or [f"migration:{mid}"]
    cdc_refs = has("cdc_snapshot:") or [f"migration:{mid}"]
    own_refs = has("ownership:") or [f"migration:{mid}"]

    status = compat.get("status")
    if status == "block":
        a["risks"].append({
            "statement": f"Compatibility is '{status}': the target plan cannot proceed until blockers are resolved.",
            "evidence_refs": compat_refs,
        })
    elif status == "conditional":
        a["warnings"].append({
            "statement": f"Compatibility is '{status}': cutover readiness requires explicit human approval of the conditions.",
            "evidence_refs": compat_refs,
        })
    elif status == "unknown":
        a["warnings"].append({
            "statement": "Compatibility is 'unknown': readiness cannot be established from current evidence.",
            "evidence_refs": compat_refs,
        })
    elif status is None:
        a["missing_evidence"].append("compatibility report (no evaluation found for this workload)")
    if plan.get("overall_status") == "block" or (plan.get("blockers") or []):
        a["risks"].append({
            "statement": "Migration plan carries blockers; provisioning must stay gated until they clear.",
            "evidence_refs": plan_refs,
        })
    if plan is None or not bundle.get("plan"):
        if "migration plan (no plan generated for this workload)" not in a["missing_evidence"]:
            a["missing_evidence"].append("migration plan (no plan generated for this workload)")

    severity = drift.get("severity") or drift.get("drift_gate")
    if severity in ("blocking", "security_critical"):
        a["risks"].append({
            "statement": f"Drift severity is '{severity}': desired and observed state disagree on migration-relevant fields.",
            "evidence_refs": drift_refs,
        })
    elif drift is None or not bundle.get("drift"):
        a["missing_evidence"].append("drift report (no evaluation found for this workload)")
    elif severity not in (None, "informational", "clear"):
        a["warnings"].append({
            "statement": f"Drift gate reports '{severity}'; re-check before cutover.",
            "evidence_refs": drift_refs,
        })

    lag, rpo = cdc.get("lag_seconds"), cdc.get("rpo_seconds") or 30
    if isinstance(lag, (int, float)) and lag is not None:
        if lag > rpo:
            a["risks"].append({
                "statement": f"CDC lag ({lag}s) exceeds the RPO threshold ({rpo}s); cutover readiness is not met.",
                "evidence_refs": cdc_refs,
            })
        elif lag >= 0.8 * rpo:
            a["warnings"].append({
                "statement": f"CDC lag ({lag}s) is close to the RPO threshold ({rpo}s); re-measure before cutover.",
                "evidence_refs": cdc_refs,
            })
    else:
        a["missing_evidence"].append("CDC lag measurement (no lag reported in current evidence)")
    if cdc.get("reconciliation_match") is False:
        a["risks"].append({
            "statement": "Reconciliation reports mismatch on the probe set; do not cut over until it matches.",
            "evidence_refs": cdc_refs,
        })

    approved = [x for x in approvals if isinstance(x, dict) and x.get("decision") == "approved"]
    pending = [x for x in approvals if isinstance(x, dict) and x.get("decision") == "pending"]
    appr_refs = [f"approval:{x['id']}" for x in approvals if isinstance(x, dict) and x.get("id")] or [f"migration:{mid}"]
    if pending:
        a["warnings"].append({
            "statement": f"{len(pending)} approval(s) pending human decision; nothing is authorized until a distinct human approves.",
            "evidence_refs": appr_refs,
        })
    if not approved and (safety.get("policy_decision") == "approval_required" or status == "conditional"):
        a["missing_evidence"].append("approved approval bound to current evidence (policy requires explicit approval)")

    if _lifecycle_status(summary, "REHEARSED") not in ("completed",):
        a["recommended_checks"].append("Run rehearsal and confirm REHEARSAL_READY before canary stages.")
    canary_items = canary.get("items") or [] if isinstance(canary, dict) else []
    if not canary_items:
        a["recommended_checks"].append("Execute read-only canary stages 0-50 with per-stage volume and windows.")
    else:
        fails = [c for c in canary_items if isinstance(c, dict) and c.get("verdict") == "FAIL"]
        if fails:
            a["risks"].append({
                "statement": f"{len(fails)} canary stage(s) reported FAIL; investigate before proceeding.",
                "evidence_refs": [f"canary:{c['id']}" for c in fails if c.get("id")] or [f"migration:{mid}"],
            })
    if _lifecycle_status(summary, "QUIESCED") != "completed":
        a["recommended_checks"].append("Verify write quiesce (503 on source writes) before final CDC catch-up.")
    owner = ownership.get("current_owner")
    if owner and owner != "aws":
        a["warnings"].append({
            "statement": f"Current authority is '{owner}': post-authority rollback is blocked by design; recovery is forward-fix only.",
            "evidence_refs": own_refs,
        })
    a["recommended_checks"].append("Re-run policy readiness immediately before cutover; measured evidence expires approvals.")

    for item in a["risks"] + a["warnings"]:
        for r in item["evidence_refs"]:
            if r not in a["evidence_references"]:
                a["evidence_references"].append(r)
    for r in ref_ids:
        if r.startswith(("compat:", "plan:", "drift:", "cdc_snapshot:", "ownership:")) and r not in a["evidence_references"]:
            a["evidence_references"].append(r)

    parts = [f"Migration {mid} review over {len(ref_ids)} evidence items."]
    if a["risks"]:
        parts.append(f"{len(a['risks'])} risk(s) identified.")
    if a["warnings"]:
        parts.append(f"{len(a['warnings'])} warning(s) identified.")
    if a["missing_evidence"]:
        parts.append(f"{len(a['missing_evidence'])} evidence gap(s) remain.")
    parts.append("Advisory only: this review authorizes nothing.")
    a["summary"] = " ".join(parts)

    if a["risks"] or status == "block":
        review["confidence"] = "low"
    elif a["warnings"] or a["missing_evidence"]:
        review["confidence"] = "medium"
    else:
        review["confidence"] = "high"
    review["authorization"] = AUTHORIZATION_NEVER
    return review


class HttpProvider:
    """Optional generic HTTP (OpenAI-compatible chat-completions) provider.

    Never used by tests or acceptance. Reads endpoint/key from config at
    call time; the key travels only in the Authorization header, never in
    the prompt, and is never logged.
    """

    def __init__(self, *, endpoint: str, api_key: str = "", model_id: str = "http-llm"):
        self.endpoint = endpoint.rstrip("/")
        self.api_key = api_key
        self.model_id = model_id or "http-llm"

    def complete(self, *, system_prompt: str, evidence_json: str, timeout: float) -> str:
        from . import sanitize as _s
        body = json.dumps({
            "model": self.model_id,
            "messages": [
                {"role": "system", "content": system_prompt},
                # Evidence travels as explicitly framed untrusted content so
                # injected instructions inside workload/application data are
                # not mistaken for operator directives.
                {"role": "user", "content": _s.frame_untrusted("EVIDENCE", evidence_json)},
            ],
            "response_format": {"type": "json_object"},
            "temperature": 0,
        }).encode("utf-8")
        headers = {"Content-Type": "application/json"}
        if self.api_key:
            headers["Authorization"] = "Bearer " + self.api_key
        req = urllib.request.Request(self.endpoint + "/chat/completions", data=body, headers=headers, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                payload = json.loads(resp.read().decode("utf-8"))
            return payload["choices"][0]["message"]["content"]
        except Exception as e:
            raise ProviderError(f"http provider call failed: {e}", kind="provider_error")


class SlowProvider:
    """Test helper: sleeps longer than any timeout to exercise timeout handling."""

    model_id = "slow-test"

    def __init__(self, delay: float = 30.0):
        self.delay = delay

    def complete(self, *, system_prompt: str, evidence_json: str, timeout: float) -> str:
        time.sleep(self.delay)
        return json.dumps(blank_review("unreached"))


def complete_with_timeout(provider, *, system_prompt: str, evidence_json: str,
                          timeout: float) -> str:
    """Run provider.complete with a hard timeout. Raises ProviderError('model_timeout')."""
    with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
        fut = pool.submit(provider.complete, system_prompt=system_prompt,
                          evidence_json=evidence_json, timeout=timeout)
        try:
            return fut.result(timeout=timeout)
        except concurrent.futures.TimeoutError:
            raise ProviderError(f"model call exceeded {timeout}s", kind="model_timeout")
        except ProviderError:
            raise
        except Exception as e:
            raise ProviderError(f"model call failed: {e}", kind="provider_error")


def build_system_prompt() -> str:
    from . import sanitize as _s
    from .schema import PROMPT_VERSION
    return (
        f"You are SKYBRIDGE {PROMPT_VERSION}, an ADVISORY migration reviewer. "
        "Rules: (1) Use ONLY the supplied evidence; never invent resource names, "
        "capabilities, metrics, approvals, or policy decisions. Every risk and "
        "warning MUST cite at least one evidence id from the supplied reference "
        "list. (2) Your JSON MUST match the required schema with "
        f'"authorization" exactly "{AUTHORIZATION_NEVER}". (3) You NEVER allow, '
        "deny, approve, or execute anything: no \"decision\" field with an "
        "authoritative value may appear. (4) " + _s.UNTRUSTED_PREAMBLE
    )
