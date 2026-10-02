import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";
import * as matchers from "vitest-axe/matchers";
import StatusBadge from "./StatusBadge";
import { CdcPanel, OwnershipPanel, SafetyPanel } from "./panels";
import { CutoverTimeline, LifecycleTimeline } from "./timelines";

expect.extend(matchers);

describe("accessibility", () => {
  it("status badge has no violations", async () => {
    const { container } = render(<StatusBadge value="CUTOVER_COMPLETE" />);
    expect(await axe(container)).toHaveNoViolations();
  });

  it("lifecycle timeline has no violations", async () => {
    const { container } = render(
      <LifecycleTimeline
        stages={[
          { name: "REGISTERED", status: "completed" },
          { name: "COMPLETE", status: "pending" },
        ]}
      />,
    );
    expect(await axe(container)).toHaveNoViolations();
  });

  it("cutover timeline has no violations", async () => {
    const { container } = render(
      <CutoverTimeline stages={[{ name: "FINAL_PREFLIGHT", status: "completed" }]} />,
    );
    expect(await axe(container)).toHaveNoViolations();
  });

  it("ownership panel has no violations", async () => {
    const { container } = render(
      <OwnershipPanel
        ownership={{
          current_owner: "aws",
          routing: "aws",
          source_writable: true,
          target_writable: false,
          split_brain: "SAFE",
        }}
      />,
    );
    expect(await axe(container)).toHaveNoViolations();
  });

  it("cdc + safety panels have no violations", async () => {
    const { container } = render(
      <>
        <CdcPanel cdc={{ source_lsn: "0/1", lag_seconds: 2, rpo_seconds: 30 }} />
        <SafetyPanel safety={{ compatibility: "conditional", rollback: "PRE_WRITE_AVAILABLE" }} />
      </>,
    );
    expect(await axe(container)).toHaveNoViolations();
  });
});
