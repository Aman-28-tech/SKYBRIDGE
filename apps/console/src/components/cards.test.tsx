import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ConnectionBanner, EmptyState, ErrorState, Loading } from "./cards";

describe("loading/error/empty/disconnected states", () => {
  it("loading announces politely", () => {
    render(<Loading label="dashboard" />);
    expect(screen.getByRole("status")).toHaveTextContent("Loading dashboard");
  });

  it("error state never implies success and offers retry", () => {
    const retry = vi.fn();
    render(<ErrorState label="Dashboard" error="control plane unreachable" onRetry={retry} />);
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("unavailable");
    expect(alert).toHaveTextContent("UNKNOWN instead of assumed values");
    expect(alert).not.toHaveTextContent("PASS");
    screen.getByRole("button", { name: "Retry" }).click();
    expect(retry).toHaveBeenCalled();
  });

  it("empty state is explicit", () => {
    render(<EmptyState label="audit entries" hint="No entries yet." />);
    expect(screen.getByText(/No audit entries yet/)).toBeInTheDocument();
  });

  it("disconnected banner warns instead of success", () => {
    render(<ConnectionBanner connected={false} updatedAt={null} stale={false} />);
    const banner = screen.getByTestId("connection-banner");
    expect(banner).toHaveTextContent("disconnected");
    expect(banner).not.toHaveTextContent("PASS");
  });

  it("stale banner warns about outdated values", () => {
    render(<ConnectionBanner connected updatedAt={Date.now() - 60000} stale />);
    expect(screen.getByTestId("connection-banner")).toHaveTextContent("stale");
  });
});
