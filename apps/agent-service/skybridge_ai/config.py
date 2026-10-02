"""Configuration for the AI planner/validator service.

All settings come from the environment (the existing SKYBRIDGE configuration
mechanism: same pattern as PORT/DATABASE_URL/TEMPORAL_HOST on the control
plane and NEXT_PUBLIC_* on the console). No API keys are hardcoded; no key
is ever required for local development, tests, or acceptance.
"""
import os


def control_plane_url() -> str:
    return os.environ.get("SKYBRIDGE_CONTROL_PLANE_URL", "http://localhost:18080").rstrip("/")


def provider_name() -> str:
    """Model provider selection. Only 'mock' is exercised by tests/acceptance."""
    return os.environ.get("SKYBRIDGE_AI_PROVIDER", "mock").strip().lower() or "mock"


def model_id() -> str:
    return os.environ.get("SKYBRIDGE_AI_MODEL_ID", "mock-deterministic-v1").strip() or "mock-deterministic-v1"


def provider_timeout_seconds() -> float:
    try:
        return max(0.5, float(os.environ.get("SKYBRIDGE_AI_TIMEOUT_SECONDS", "10")))
    except ValueError:
        return 10.0


def provider_endpoint() -> str:
    """Optional generic HTTP LLM endpoint (OpenAI-compatible chat completions)."""
    return os.environ.get("SKYBRIDGE_AI_ENDPOINT", "").strip()


def provider_api_key() -> str:
    """Bearer key for the optional HTTP provider. Never logged, never sent to the model input."""
    return os.environ.get("SKYBRIDGE_AI_API_KEY", "")


def service_port() -> int:
    try:
        return int(os.environ.get("SKYBRIDGE_AI_PORT", "18082"))
    except ValueError:
        return 18082
