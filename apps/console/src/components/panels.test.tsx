import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { CdcPanel, OwnershipPanel, SafetyPanel } from "./panels";

describe("OwnershipPanel", () => {
  it("explains why a non-authoritative provider rejects writes", () => {
    render(
      <OwnershipPanel
        ownership={{
          current_owner: "azure",
          routing: "azure",
          source_writable: false,
          target_writable: true,
          split_brain: "SAFE",
          source_explanation: "Source (AWS) rejects writes with 403 WRITE_NOT_OWNED: it is not authoritative (current authority: azure).",
          target_explanation: "Target (Azure) accepts writes: it is the authoritative provider.",
        }}
      />,
    );
    expect(screen.getByTestId("ownership-panel")).toBeInTheDocument();
    expect(screen.getByText(/rejects writes with 403/)).toBeInTheDocument();
    expect(screen.getByText("SAFE")).toBeInTheDocument();
  });

  it("renders UNKNOWN when disconnected", () => {
    render(<OwnershipPanel ownership={null} />);
    expect(screen.getByText(/UNKNOWN/)).toBeInTheDocument();
  });
});

describe("CdcPanel", () => {
  it("shows UNKNOWN instead of fabricated history", () => {
    render(
      <CdcPanel
        cdc={{
          source_lsn: "",
          applied_lsn: "",
          lag_seconds: null,
          rpo_seconds: 30,
          events_captured: null,
          events_applied: null,
          events_duplicates: null,
          reconciliation_match: null,
        }}
      />,
    );
    expect(screen.getAllByText("UNKNOWN").length).toBeGreaterThan(0);
  });

  it("renders measured lag and counts", () => {
    render(
      <CdcPanel
        cdc={{
          source_lsn: "0/A001",
          applied_lsn: "0/A001",
          lag_seconds: 2,
          rpo_seconds: 30,
          events_captured: 7,
          events_applied: 5,
          events_duplicates: 2,
          reconciliation_match: true,
        }}
      />,
    );
    expect(screen.getAllByText("0/A001").length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText("7", { exact: false })).toBeInTheDocument();
    expect(screen.getByText("MATCH")).toBeInTheDocument();
  });
});

describe("SafetyPanel", () => {
  it("marks blocked rollback as obvious", () => {
    render(
      <SafetyPanel
        safety={{
          compatibility: "conditional",
          drift_gate: "clear",
          policy_decision: "allow",
          approval: "appr-1",
          canary_verdict: "PASS",
          canary_stage: 50,
          quiesce: "quiesced",
          rollback: "POST_WRITE_ROLLBACK_BLOCKED",
          split_brain: "SAFE",
        }}
      />,
    );
    expect(screen.getByTestId("safety-panel")).toBeInTheDocument();
    const row = screen.getByTestId("safety-Rollback");
    expect(row).toHaveClass("safety-blocked");
    expect(screen.getByText(/forward-fix/)).toBeInTheDocument();
  });
});
