import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { CutoverTimeline, LifecycleTimeline } from "./timelines";
import { CUTOVER_ORDER, LIFECYCLE_ORDER } from "@/lib/types";

describe("LifecycleTimeline", () => {
  it("highlights only the API-reported current stage", () => {
    const stages = LIFECYCLE_ORDER.map((name, i) => ({
      name,
      status: (i < 3 ? "completed" : i === 3 ? "current" : "pending") as
        | "completed"
        | "current"
        | "pending",
    }));
    render(<LifecycleTimeline stages={stages} />);
    const tl = screen.getByTestId("lifecycle-timeline");
    expect(within(tl).getAllByRole("listitem")).toHaveLength(8);
    expect(screen.getByTestId("lifecycle-REHEARSED")).toHaveClass("stage-current");
    expect(screen.getByTestId("lifecycle-REGISTERED")).toHaveClass("stage-done");
    expect(screen.getByTestId("lifecycle-COMPLETE")).toHaveClass("stage-pending");
  });

  it("never claims completion without evidence", () => {
    const stages = LIFECYCLE_ORDER.map((name) => ({
      name,
      status: (name === "REGISTERED" ? "completed" : "pending") as "completed" | "pending",
    }));
    render(<LifecycleTimeline stages={stages} />);
    for (const name of LIFECYCLE_ORDER.slice(1)) {
      expect(screen.getByTestId(`lifecycle-${name}`)).toHaveClass("stage-pending");
    }
  });
});

describe("CutoverTimeline", () => {
  it("renders all 9 stages with metadata", () => {
    const stages = CUTOVER_ORDER.map((name, i) => ({
      name,
      status: (i === 0 ? "completed" : i === 1 ? "current" : "pending") as
        | "completed"
        | "current"
        | "pending",
      timestamp: i === 0 ? "2026-01-01T00:00:00Z" : undefined,
      request_id: i === 0 ? "req-1" : undefined,
      actor: i === 0 ? "operator" : undefined,
      policy_hash: i === 0 ? "abc123" : undefined,
      approval_id: undefined,
      source_lsn: i === 0 ? "0/A001" : undefined,
      applied_lsn: undefined,
      lag_seconds: i === 0 ? 2 : undefined,
    }));
    render(<CutoverTimeline stages={stages} />);
    expect(screen.getByTestId("cutover-timeline").children).toHaveLength(9);
    expect(screen.getByTestId("cutover-FINAL_PREFLIGHT")).toHaveClass("stage-done");
    expect(screen.getByTestId("cutover-WRITES_QUIESCED")).toHaveClass("stage-current");
    expect(screen.getByText("req-1")).toBeInTheDocument();
    expect(screen.getByText("0/A001")).toBeInTheDocument();
  });
});
