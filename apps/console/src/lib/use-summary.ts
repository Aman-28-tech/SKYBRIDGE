"use client";

import { useCallback, useEffect, useState } from "react";
import { useSearchParams } from "next/navigation";
import {
  apiAudit,
  apiCanary,
  apiCutoverStatus,
  apiHealth,
  apiSummary,
  resolveIds,
} from "./api";
import { POLL_INTERVAL_MS } from "./config";
import { defaultMigrationId, defaultWorkloadId } from "./config";
import type { AuditItem, FetchState, SummaryResponse } from "./types";

export type DashboardData = {
  summary: SummaryResponse | null;
  audit: AuditItem[];
  canary: { items: Array<Record<string, unknown>>; expected_stage: number } | null;
  cutoverStatus: Record<string, unknown> | null;
  health: string | null;
  workloadId: string | null;
  migrationId: string | null;
};

export function useConsoleIds(): { workloadId: string | null; migrationId: string | null } {
  const params = useSearchParams();
  const [ids, setIds] = useState<{ workloadId: string | null; migrationId: string | null }>({
    workloadId: params.get("workload") || defaultWorkloadId() || null,
    migrationId: params.get("migration") || defaultMigrationId() || null,
  });
  useEffect(() => {
    setIds({
      workloadId: params.get("workload") || defaultWorkloadId() || null,
      migrationId: params.get("migration") || defaultMigrationId() || null,
    });
  }, [params]);
  return ids;
}

export function useDashboard(
  poll = true,
  hintOverride?: { workloadId?: string | null; migrationId?: string | null },
): FetchState<DashboardData> & { refresh: () => void } {
  const { workloadId: hintW, migrationId: hintM } = useConsoleIds();
  const workloadHint = hintOverride?.workloadId ?? hintW;
  const migrationHint = hintOverride?.migrationId ?? hintM;
  const [state, setState] = useState<FetchState<DashboardData>>({
    data: null,
    error: null,
    loading: true,
    updatedAt: null,
    stale: false,
  });

  const load = useCallback(async () => {
    setState((s) => ({ ...s, loading: s.data === null, error: null }));
    const resolved = await resolveIds(workloadHint || undefined, migrationHint || undefined);
    if (!resolved.workloadId || !resolved.migrationId) {
      setState({
        data: null,
        error: resolved.error || "no workload/migration selected",
        loading: false,
        updatedAt: Date.now(),
        stale: false,
      });
      return;
    }
    const [summary, audit, canary, cutoverStatus, health] = await Promise.all([
      apiSummary(resolved.migrationId),
      apiAudit(resolved.migrationId),
      apiCanary(resolved.migrationId),
      apiCutoverStatus(resolved.migrationId),
      apiHealth(),
    ]);
    const errors = [summary.error, audit.error, canary.error, cutoverStatus.error, health.error].filter(Boolean);
    // Summary is the primary source; audit/cutover/canary enrich it.
    // If the summary itself fails, the whole view is disconnected (no fake success).
    if (summary.error || !summary.data) {
      setState({
        data: null,
        error: summary.error || "summary unavailable",
        loading: false,
        updatedAt: Date.now(),
        stale: false,
      });
      return;
    }
    setState({
      data: {
        summary: summary.data,
        audit: audit.data?.items || [],
        canary: canary.data
          ? { items: (canary.data.items as Array<Record<string, unknown>>) || [], expected_stage: canary.data.expected_stage }
          : null,
        cutoverStatus: (cutoverStatus.data as Record<string, unknown>) || null,
        health: health.data,
        workloadId: resolved.workloadId,
        migrationId: resolved.migrationId,
      },
      // Surface partial failures without hiding the primary summary.
      error: errors.length > 0 ? errors.join("; ") : null,
      loading: false,
      updatedAt: Date.now(),
      stale: false,
    });
  }, [workloadHint, migrationHint]);

  useEffect(() => {
    load();
  }, [load]);

  useEffect(() => {
    if (!poll) return;
    const t = setInterval(() => {
      setState((s) => {
        if (s.updatedAt && Date.now() - s.updatedAt > 30000) {
          return { ...s, stale: true };
        }
        return s;
      });
    }, 5000);
    return () => clearInterval(t);
  }, [poll]);

  useEffect(() => {
    if (!poll) return;
    const t = setInterval(load, POLL_INTERVAL_MS);
    return () => clearInterval(t);
  }, [poll, load]);

  return { ...state, refresh: load };
}
