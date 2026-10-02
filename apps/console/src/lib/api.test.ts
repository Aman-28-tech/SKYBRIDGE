import { afterEach, describe, expect, it, vi } from "vitest";
import { apiAudit, apiSummary, displayOrUnknown, resolveIds } from "./api";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.resetAllMocks();
});

function mockFetchOnce(body: unknown, status = 200) {
  const mock = vi.fn().mockResolvedValue({
    ok: status >= 200 && status < 300,
    status,
    statusText: status === 200 ? "OK" : "Error",
    json: async () => body,
    text: async () => JSON.stringify(body),
  });
  vi.stubGlobal("fetch", mock);
  return mock;
}

describe("read-only api client", () => {
  it("returns data on 200 without inventing values", async () => {
    mockFetchOnce({ migration_id: "m1", lifecycle: { current: "COMPLETE" } });
    const r = await apiSummary("m1");
    expect(r.error).toBeNull();
    expect(r.data?.migration_id).toBe("m1");
  });

  it("returns 404 as error (never empty-timeline misread)", async () => {
    mockFetchOnce({ error: { code: "NOT_FOUND", message: "migration not found" } }, 404);
    const r = await apiAudit("missing");
    expect(r.data).toBeNull();
    expect(r.error).toMatch(/404/);
  });

  it("returns network failure as disconnected (never fake success)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("boom")));
    const r = await apiSummary("m1");
    expect(r.data).toBeNull();
    expect(r.error).toMatch(/unreachable/);
  });

  it("resolveIds never invents IDs when no workloads exist", async () => {
    mockFetchOnce({ items: [] });
    const r = await resolveIds(undefined, undefined);
    expect(r.migrationId).toBeNull();
    expect(r.error).toMatch(/no workloads/);
  });

  it("displayOrUnknown renders UNKNOWN for missing values", () => {
    expect(displayOrUnknown(null)).toBe("UNKNOWN");
    expect(displayOrUnknown("")).toBe("UNKNOWN");
    expect(displayOrUnknown("0/A001")).toBe("0/A001");
  });
});
