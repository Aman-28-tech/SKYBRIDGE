import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import StatusBadge from "./StatusBadge";

describe("StatusBadge", () => {
  it("renders UNKNOWN for empty values (never fake success)", () => {
    render(<StatusBadge value={null} />);
    expect(screen.getByRole("status")).toHaveTextContent("UNKNOWN");
  });

  it("marks CUTOVER_COMPLETE as ok", () => {
    render(<StatusBadge value="CUTOVER_COMPLETE" />);
    expect(screen.getByRole("status")).toHaveClass("badge-ok");
  });

  it("marks blocked states as bad", () => {
    render(<StatusBadge value="CUTOVER_BLOCKED" />);
    expect(screen.getByRole("status")).toHaveClass("badge-bad");
  });

  it("marks unknown/pending as warn", () => {
    render(<StatusBadge value="UNKNOWN" />);
    expect(screen.getByRole("status")).toHaveClass("badge-warn");
  });
});
