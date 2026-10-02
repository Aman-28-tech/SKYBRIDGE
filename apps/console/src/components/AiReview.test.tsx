import { render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { apiAiReview } from "@/lib/api";
import type { AiReviewEnvelope } from "@/lib/types";
import { AiReviewPanel } from "./AiReview";

vi.mock("@/lib/api", () => ({
  apiAiReview: vi.fn(),
}));

const mocked = vi.mocked(apiAiReview);

const ENVELOPE: AiReviewEnvelope = {
  request_id: "req_test",
  migration_id: "m1",
  workload_id: "w1",
  target_provider: "azure",
  review: {
    migration_id: "m1",
    assessment: {
      summary: "Migration m1 review over 5 evidence items. Advisory only.",
      risks: [{ statement: "Drift severity is 'blocking'.", evidence_refs: ["drift:d1"] }],
      warnings: [{ statement: "Compatibility is 'conditional'.", evidence_refs: ["compat:c1"] }],
      missing_evidence: ["approved approval bound to current evidence"],
      recommended_checks: ["Re-run policy readiness immediately before cutover."],
      evidence_references: ["drift:d1", "compat:c1"],
    },
    proposed_plan_changes: [],
    confidence: "medium",
    authorization: "NEVER_BY_AI",
  },
  prompt_version: "ai-planner-v1",
  model_id: "mock-deterministic-v1",
  evidence_fingerprint: "sha256:abc",
  evidence: [],
  validation_result: "valid",
  error: null,
};

beforeEach(() => {
  vi.resetAllMocks();
});

describe("AiReviewPanel", () => {
  it("renders summary, risks, warnings, evidence and the advisory label", async () => {
    mocked.mockResolvedValue({ data: ENVELOPE, error: null });
    render(<AiReviewPanel migrationId="m1" />);
    expect(await screen.findByTestId("ai-review-panel")).toBeInTheDocument();
    expect(screen.getByText(/ADVISORY — DOES NOT AUTHORIZE MIGRATION/)).toBeInTheDocument();
    expect(screen.getByTestId("ai-summary")).toHaveTextContent("Advisory only");
    expect(screen.getByTestId("ai-risks")).toHaveTextContent("blocking");
    expect(screen.getByTestId("ai-warnings")).toHaveTextContent("conditional");
    expect(screen.getAllByText("NEVER_BY_AI").length).toBeGreaterThan(0);
    expect(screen.getByText("ai-planner-v1")).toBeInTheDocument();
  });

  it("shows unavailable instead of a fabricated review when the service is down", async () => {
    mocked.mockResolvedValue({ data: null, error: "AI service unreachable at x" });
    render(<AiReviewPanel migrationId="m1" />);
    expect(await screen.findByText(/AI review unavailable/)).toBeInTheDocument();
    expect(screen.queryByTestId("ai-summary")).not.toBeInTheDocument();
  });

  it("compact mode shows summary and confidence without detail lists", async () => {
    mocked.mockResolvedValue({ data: ENVELOPE, error: null });
    render(<AiReviewPanel migrationId="m1" compact />);
    expect(await screen.findByTestId("ai-summary")).toBeInTheDocument();
    expect(screen.queryByTestId("ai-risks")).not.toBeInTheDocument();
  });
});
