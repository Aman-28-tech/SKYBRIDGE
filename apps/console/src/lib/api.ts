// Read-only control-plane client. GET only: the console never issues
// mutations (no POST/PUT/PATCH/DELETE, no Terraform Apply, no ownership
// transfer, no rollback). Every helper returns null data + an error string
// on failure so the UI renders UNKNOWN/disconnected instead of fake success.
import { aiServiceUrl, controlPlaneUrl } from "./config";
import type { AiReviewEnvelope, AuditItem, SummaryResponse } from "./types";

export type ApiResult<T> = { data: T | null; error: string | null };

async function getJsonFrom<T>(base: string, path: string, service: string): Promise<ApiResult<T>> {
  let res: Response;
  try {
    res = await fetch(`${base}${path}`, {
      method: "GET",
      cache: "no-store",
      headers: { Accept: "application/json" },
    });
  } catch (e) {
    return {
      data: null,
      error: `${service} unreachable at ${base}: ${e instanceof Error ? e.message : String(e)}`,
    };
  }
  if (!res.ok) {
    let detail = "";
    try {
      const body = await res.json();
      const err = (body as Record<string, unknown>)?.error as
        | Record<string, unknown>
        | undefined;
      detail =
        (err?.message as string) || (err?.code as string) || res.statusText;
    } catch {
      detail = res.statusText;
    }
    return { data: null, error: `GET ${path}: ${res.status} ${detail}`.trim() };
  }
  try {
    const data = (await res.json()) as T;
    return { data, error: null };
  } catch (e) {
    return {
      data: null,
      error: `GET ${path}: invalid JSON (${e instanceof Error ? e.message : String(e)})`,
    };
  }
}

async function getJson<T>(path: string): Promise<ApiResult<T>> {
  return getJsonFrom<T>(controlPlaneUrl(), path, "control plane");
}

export function apiHealth() {
  const base = controlPlaneUrl();
  return fetch(`${base}/healthz`, { cache: "no-store" })
    .then(async (res) => {
      if (!res.ok) return { data: null as string | null, error: `healthz: ${res.status}` };
      const text = await res.text();
      return { data: text, error: null as string | null };
    })
    .catch((e: unknown) => ({
      data: null as string | null,
      error: `control plane unreachable at ${base}: ${e instanceof Error ? e.message : String(e)}`,
    }));
}

export function apiWorkloads() {
  return getJson<{ items: Array<Record<string, unknown>> }>(`/v1/workloads`);
}

export function apiWorkload(workloadId: string) {
  return getJson<Record<string, unknown>>(
    `/v1/workloads/${encodeURIComponent(workloadId)}`,
  );
}

export function apiMigrationsForWorkload(workloadId: string) {
  return getJson<{
    workload_id: string;
    items: Array<Record<string, unknown>>;
  }>(`/v1/workloads/${encodeURIComponent(workloadId)}/migrations`);
}

export function apiMigration(migrationId: string) {
  return getJson<Record<string, unknown>>(
    `/v1/migrations/${encodeURIComponent(migrationId)}`,
  );
}

export function apiSummary(migrationId: string) {
  return getJson<SummaryResponse>(
    `/v1/migrations/${encodeURIComponent(migrationId)}/summary`,
  );
}

export function apiAudit(migrationId: string) {
  return getJson<{ migration_id: string; items: AuditItem[] }>(
    `/v1/migrations/${encodeURIComponent(migrationId)}/audit`,
  );
}

export function apiCutoverStatus(migrationId: string) {
  return getJson<Record<string, unknown>>(
    `/v1/migrations/${encodeURIComponent(migrationId)}/cutover`,
  );
}

export function apiCanary(migrationId: string) {
  return getJson<{
    items: Array<Record<string, unknown>>;
    latest: Record<string, unknown>;
    expected_stage: number;
  }>(`/v1/migrations/${encodeURIComponent(migrationId)}/canary`);
}

export function apiCompat(workloadId: string) {
  return getJson<Record<string, unknown>>(
    `/v1/workloads/${encodeURIComponent(workloadId)}/compatibility`,
  );
}

export function apiPlan(workloadId: string) {
  return getJson<Record<string, unknown>>(
    `/v1/workloads/${encodeURIComponent(workloadId)}/plan`,
  );
}

export function apiDrift(workloadId: string) {
  return getJson<{ items: Array<Record<string, unknown>> }>(
    `/v1/workloads/${encodeURIComponent(workloadId)}/drift`,
  );
}

export function apiApprovals(workloadId: string) {
  return getJson<{ items: Array<Record<string, unknown>> }>(
    `/v1/workloads/${encodeURIComponent(workloadId)}/approvals`,
  );
}

// Advisory AI review (GET only). The AI service reads control-plane
// evidence and returns a validated advisory envelope; the console never
// sends mutations through this client.
export function apiAiReview(migrationId: string) {
  return getJsonFrom<AiReviewEnvelope>(
    aiServiceUrl(),
    `/v1/ai/migration-review?migration_id=${encodeURIComponent(migrationId)}&target_provider=azure`,
    "AI service",
  );
}

// Resolve the dashboard workload/migration pair: explicit IDs win, otherwise
// discover the first workload and its first migration. Never invents IDs.
export async function resolveIds(
  workloadId?: string,
  migrationId?: string,
): Promise<{
  workloadId: string | null;
  migrationId: string | null;
  error: string | null;
}> {
  if (workloadId && migrationId) {
    return { workloadId, migrationId, error: null };
  }
  const wl = await apiWorkloads();
  if (wl.error || !wl.data) {
    return {
      workloadId: null,
      migrationId: null,
      error: wl.error || "no workloads returned",
    };
  }
  const items = wl.data.items || [];
  if (items.length === 0) {
    return { workloadId: null, migrationId: null, error: "no workloads registered" };
  }
  const wid =
    workloadId || (items[0] as Record<string, unknown>).id as string || null;
  if (!wid) {
    return { workloadId: null, migrationId: null, error: "workload has no id" };
  }
  if (migrationId) return { workloadId: wid, migrationId, error: null };
  const migs = await apiMigrationsForWorkload(wid);
  if (migs.error || !migs.data) {
    return {
      workloadId: wid,
      migrationId: null,
      error: migs.error || "no migrations returned",
    };
  }
  const mitems = migs.data.items || [];
  if (mitems.length === 0) {
    return { workloadId: wid, migrationId: null, error: "no migrations for workload" };
  }
  const mid = (mitems[0] as Record<string, unknown>).id as string;
  return { workloadId: wid, migrationId: mid || null, error: mid ? null : "migration has no id" };
}

export function displayOrUnknown(v: unknown): string {
  if (v === null || v === undefined || v === "") return "UNKNOWN";
  if (typeof v === "number") return String(v);
  return String(v);
}
