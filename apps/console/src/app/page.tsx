"use client";

import { Suspense } from "react";
import { ConnectionBanner, EmptyState, ErrorState, Loading, MetricRow, SectionCard } from "@/components/cards";
import { CdcPanel, OwnershipPanel, SafetyPanel } from "@/components/panels";
import StatusBadge from "@/components/StatusBadge";
import { LifecycleTimeline } from "@/components/timelines";
import { useDashboard } from "@/lib/use-summary";

function DashboardInner() {
  const { data, error, loading, updatedAt, stale, refresh } = useDashboard(true);

  if (loading && !data) return <Loading label="dashboard" />;
  if (!data) {
    return (
      <>
        <ConnectionBanner connected={false} updatedAt={updatedAt} stale={stale} />
        <ErrorState label="Dashboard" error={error || "control plane unavailable"} onRetry={refresh} />
      </>
    );
  }

  const s = data.summary!;
  const connected = !error || error.indexOf("control plane unreachable") === -1;

  return (
    <>
      <div className="page-head">
        <h2>Dashboard</h2>
        <p className="muted">
          Read-only view of the SKYBRIDGE control plane. Refreshes from live APIs every 5s.
          {updatedAt ? ` Last update: ${new Date(updatedAt).toISOString()}` : ""}
        </p>
      </div>
      <ConnectionBanner connected={connected} updatedAt={updatedAt} stale={stale} />
      {error ? (
        <div className="banner banner-warn" data-testid="partial-error">
          Partial data: {error}
        </div>
      ) : null}

      <div className="grid grid-2">
        <SectionCard title="System" testId="system-panel">
          <MetricRow label="SKYBRIDGE status" badge={data.health === "ok" ? "ok" : data.health ? data.health : "UNKNOWN"} />
          <MetricRow label="Mode" badge={s.mode?.local_demo ? "LOCAL DEMO" : "UNKNOWN"} />
          <MetricRow label="Source provider" badge={s.providers?.source_provider || "UNKNOWN"} />
          <MetricRow label="Target provider" badge={s.providers?.target_provider || "UNKNOWN"} />
          <MetricRow label="Authoritative provider" badge={s.providers?.authoritative_provider || "UNKNOWN"} />
          <MetricRow label="Routing provider" badge={s.providers?.routing_provider || "UNKNOWN"} />
        </SectionCard>

        <SectionCard title="Migration" testId="migration-panel">
          <MetricRow label="Workload" value={`${s.workload?.name || "UNKNOWN"} (${s.workload_id})`} />
          <MetricRow label="Migration ID" value={s.migration_id} />
          <MetricRow label="Lifecycle" badge={s.lifecycle?.current || "UNKNOWN"} />
          <MetricRow label="Cutover state" badge={s.cutover?.status || "UNKNOWN"} />
          <MetricRow label="Current stage" badge={s.cutover?.current_stage || "UNKNOWN"} />
          <p className="muted small">
            <a href={`/migrations/${s.migration_id}`}>Open migration detail →</a>
          </p>
        </SectionCard>
      </div>

      <div className="grid grid-2" style={{ marginTop: 16 }}>
        <CdcPanel cdc={s.cdc || null} />
        <SafetyPanel safety={s.safety || null} />
      </div>

      <div className="grid grid-2" style={{ marginTop: 16 }}>
        <OwnershipPanel ownership={s.ownership || null} />
        <SectionCard title="Lifecycle" subtitle="A stage is completed only when the API reports it." testId="lifecycle-panel">
          {s.lifecycle?.stages ? (
            <LifecycleTimeline stages={s.lifecycle.stages} />
          ) : (
            <EmptyState label="lifecycle" hint="No lifecycle reported by the API." />
          )}
        </SectionCard>
      </div>

      <SectionCard title="Cutover evidence" subtitle="Latest audit entries (chronological)." testId="audit-panel">
        {data.audit.length === 0 ? (
          <EmptyState label="audit entries" hint="No audit entries for this migration yet." />
        ) : (
          <table className="data">
            <thead>
              <tr>
                <th>Time</th>
                <th>Action</th>
                <th>Result</th>
                <th>Actor</th>
                <th>Request</th>
              </tr>
            </thead>
            <tbody>
              {data.audit.slice(-8).map((a, i) => (
                <tr key={`${a.request_id}-${i}`}>
                  <td>{a.created_at || "—"}</td>
                  <td><code>{a.action}</code></td>
                  <td><StatusBadge value={a.result} /></td>
                  <td>{a.actor_id || "—"}</td>
                  <td><code className="wrap">{a.request_id || "—"}</code></td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </SectionCard>
    </>
  );
}

export default function DashboardPage() {
  return (
    <Suspense fallback={<Loading label="dashboard" />}>
      <DashboardInner />
    </Suspense>
  );
}
