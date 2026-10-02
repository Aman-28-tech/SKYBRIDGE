"use client";

import { Suspense } from "react";
import { ConnectionBanner, EmptyState, ErrorState, Loading, SectionCard } from "@/components/cards";
import StatusBadge from "@/components/StatusBadge";
import { CutoverTimeline } from "@/components/timelines";
import { useDashboard } from "@/lib/use-summary";

function Inner() {
  const { data, error, loading, updatedAt, stale, refresh } = useDashboard(true);
  if (loading && !data) return <Loading label="cutover timeline" />;
  if (!data || !data.summary) {
    return (
      <>
        <ConnectionBanner connected={false} updatedAt={updatedAt} stale={stale} />
        <ErrorState label="Cutover timeline" error={error || "cutover unavailable"} onRetry={refresh} />
      </>
    );
  }
  const stages = data.summary.cutover?.stages || [];
  return (
    <>
      <div className="page-head">
        <h2>Cutover timeline</h2>
        <p className="muted">Every stage with status, timestamp, request, actor, policy, approval, CDC.</p>
      </div>
      <ConnectionBanner connected={!error} updatedAt={updatedAt} stale={stale} />
      <SectionCard title="Timeline" testId="cutover-timeline-panel">
        {stages.length === 0 ? <EmptyState label="cutover stages" /> : <CutoverTimeline stages={stages} />}
      </SectionCard>
      <SectionCard title="Stage table" testId="cutover-table-panel">
        <table className="data">
          <thead>
            <tr>
              <th>Stage</th>
              <th>Status</th>
              <th>Timestamp</th>
              <th>Request ID</th>
              <th>Actor</th>
              <th>Policy hash</th>
              <th>Approval</th>
              <th>CDC</th>
              <th>Lag</th>
            </tr>
          </thead>
          <tbody>
            {stages.map((s) => (
              <tr key={s.name} data-testid={`cutover-table-${s.name}`}>
                <td><code>{s.name}</code></td>
                <td><StatusBadge value={s.status} /></td>
                <td>{s.timestamp || "—"}</td>
                <td><code className="wrap">{s.request_id || "—"}</code></td>
                <td>{s.actor || "—"}</td>
                <td><code className="wrap">{s.policy_hash || "—"}</code></td>
                <td><code className="wrap">{s.approval_id || "—"}</code></td>
                <td><code>{s.source_lsn && s.applied_lsn ? `${s.source_lsn} → ${s.applied_lsn}` : "—"}</code></td>
                <td>{s.lag_seconds === null || s.lag_seconds === undefined ? "—" : `${s.lag_seconds}s`}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </SectionCard>
    </>
  );
}

export default function CutoverPage() {
  return (
    <Suspense fallback={<Loading label="cutover timeline" />}>
      <Inner />
    </Suspense>
  );
}
