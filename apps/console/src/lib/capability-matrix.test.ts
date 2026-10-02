import { describe, expect, it } from "vitest";
import { CAPABILITY_MATRIX, EVIDENCE_BOUNDARY } from "./capability-matrix";

describe("capability matrix", () => {
  it("uses only the canonical statuses", () => {
    for (const row of CAPABILITY_MATRIX) {
      expect(["PROVEN", "PARTIALLY PROVEN", "DEFERRED"]).toContain(row.status);
    }
  });

  it("covers all three groups", () => {
    expect(CAPABILITY_MATRIX.some((r) => r.status === "PROVEN")).toBe(true);
    expect(CAPABILITY_MATRIX.some((r) => r.status === "PARTIALLY PROVEN")).toBe(true);
    expect(CAPABILITY_MATRIX.some((r) => r.status === "DEFERRED")).toBe(true);
  });

  it("states the no-mutation boundary explicitly", () => {
    expect(EVIDENCE_BOUNDARY.noMutation).toMatch(/No real cloud mutation/);
    expect(EVIDENCE_BOUNDARY.noMutation).toMatch(/No cloud spending/);
    expect(EVIDENCE_BOUNDARY.noMutation).toMatch(/Local demo environment/);
    expect(EVIDENCE_BOUNDARY.terraformApply).toMatch(/DISABLED/);
  });
});
