"""Review orchestration: evidence -> prompt -> model -> validate -> envelope.

The control plane stays authoritative: this module only reads evidence
(GET), asks the model for an advisory summary, and validates the answer.
Validation failure, provider error, or timeout yields a fail-safe
envelope with ``review: null`` -- never a fabricated review, never an
authorization.
"""
import json
import uuid

from . import config as _cfg
from . import sanitize as _san
from .evidence import (
    EvidenceError,
    canonical_fingerprint,
    evidence_id_set,
    evidence_references,
    fetch_evidence,
)
from .provider import (
    DeterministicMockProvider,
    HttpProvider,
    ProviderError,
    build_system_prompt,
    complete_with_timeout,
)
from .schema import PROMPT_VERSION
from .validate import parse_and_validate


def _request_id() -> str:
    return "req_" + uuid.uuid4().hex[:8]


def select_provider(explicit=None):
    if explicit is not None:
        return explicit
    name = _cfg.provider_name()
    if name == "mock":
        return DeterministicMockProvider()
    if name == "http":
        endpoint = _cfg.provider_endpoint()
        if not endpoint:
            raise ProviderError("SKYBRIDGE_AI_ENDPOINT is not configured", kind="provider_error")
        return HttpProvider(endpoint=endpoint, api_key=_cfg.provider_api_key(),
                            model_id=_cfg.model_id())
    raise ProviderError(f"unknown SKYBRIDGE_AI_PROVIDER={name!r}", kind="provider_error")


def build_review_envelope(*, workload_id: str, migration_id: str, target_provider: str,
                          base_url: str = None, provider=None,
                          timeout: float = None, request_id: str = None) -> tuple:
    """Run one advisory review. Returns (http_status, envelope dict)."""
    request_id = request_id or _request_id()
    base_url = (base_url or _cfg.control_plane_url()).rstrip("/")
    timeout = timeout if timeout is not None else _cfg.provider_timeout_seconds()

    if target_provider != "azure":
        return 400, _fail_envelope(request_id, migration_id, workload_id, target_provider,
                                   "invalid_input: target_provider must be 'azure'",
                                   model_id=getattr(provider, "model_id", _cfg.model_id()))

    try:
        bundle = fetch_evidence(base_url, workload_id, migration_id, target_provider, timeout=timeout)
    except EvidenceError as e:
        status = 404 if e.kind == "not_found" else 502
        if e.kind == "unreachable":
            status = 502
        return status, _fail_envelope(request_id, migration_id, workload_id, target_provider,
                                      f"{e.kind}: {e}",
                                      model_id=getattr(provider, "model_id", _cfg.model_id()),
                                      fingerprint=None)

    refs = evidence_references(bundle)
    known_ids = evidence_id_set(bundle)
    fingerprint = bundle.get("evidence_fingerprint") or canonical_fingerprint(bundle)

    try:
        active = select_provider(provider)
    except ProviderError as e:
        return 502, _fail_envelope(request_id, migration_id, workload_id, target_provider,
                                   f"{e.kind}: {e}", model_id="unconfigured", fingerprint=fingerprint,
                                   evidence_refs=refs)
    model_id = getattr(active, "model_id", _cfg.model_id())

    system_prompt = build_system_prompt()
    evidence_json = json.dumps(bundle, sort_keys=True, separators=(",", ":"), default=str)
    try:
        raw = complete_with_timeout(active, system_prompt=system_prompt,
                                    evidence_json=evidence_json, timeout=timeout)
    except ProviderError as e:
        code = 504 if e.kind == "model_timeout" else 502
        return code, _fail_envelope(request_id, migration_id, workload_id, target_provider,
                                    f"{e.kind}: {e}", model_id=model_id, fingerprint=fingerprint,
                                    evidence_refs=refs)

    review, ok, reason = parse_and_validate(raw, known_ids)
    if not ok:
        return 502, _fail_envelope(request_id, migration_id, workload_id, target_provider,
                                   f"invalid_model_output: {reason}", model_id=model_id,
                                   fingerprint=fingerprint, evidence_refs=refs)
    if review.get("migration_id") != migration_id:
        return 502, _fail_envelope(request_id, migration_id, workload_id, target_provider,
                                   "invalid_model_output: review migration_id mismatch",
                                   model_id=model_id, fingerprint=fingerprint, evidence_refs=refs)

    return 200, {
        "request_id": request_id,
        "migration_id": migration_id,
        "workload_id": workload_id,
        "target_provider": target_provider,
        "review": review,
        "prompt_version": PROMPT_VERSION,
        "model_id": model_id,
        "evidence_fingerprint": fingerprint,
        "evidence": refs,
        "validation_result": "valid",
        "error": None,
    }


def _fail_envelope(request_id, migration_id, workload_id, target_provider, error,
                   model_id="unknown", fingerprint=None, evidence_refs=None) -> dict:
    return {
        "request_id": request_id,
        "migration_id": migration_id,
        "workload_id": workload_id,
        "target_provider": target_provider,
        "review": None,
        "prompt_version": PROMPT_VERSION,
        "model_id": model_id,
        "evidence_fingerprint": fingerprint,
        "evidence": evidence_refs or [],
        "validation_result": "failed" if error.startswith(("provider", "model_", "unreachable",
                                                            "evidence_error")) else "invalid",
        "error": error,
    }
