"""Agent-service baseline: evidence-cited proposal envelope (Phase 10 full agents)."""
from dataclasses import dataclass, field


@dataclass
class Proposal:
    run_id: str
    model_id: str = "unspecified"
    prompt_name: str = "unspecified"
    prompt_version: str = "v0"
    tool_schema_version: str = "v1"
    policy_bundle_version: str = "unspecified"
    evidence_ids: list = field(default_factory=list)
    content: dict = field(default_factory=dict)

    def valid(self) -> bool:
        # Every factual claim must cite evidence; empty content with no claims is valid baseline.
        return isinstance(self.evidence_ids, list)
