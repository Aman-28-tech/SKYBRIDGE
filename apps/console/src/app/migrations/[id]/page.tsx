"use client";

import { Suspense } from "react";
import { useParams } from "next/navigation";
import {
  ConnectionBanner,
  EmptyState,
  ErrorState,
  Loading,
  MetricRow,
  SectionCard,
} from "@/components/cards";
import StatusBadge from "@/components/StatusBadge";
import { AiReviewPanel } from "@/components/AiReview";
import { CutoverTimeline, LifecycleTimeline } from "@/components/timelines";
import { useDashboard } from "@/lib/use-summary";

function DetailInner({ id }: { id: string }) {
  // The path ID is authoritative: the detail view must render THIS
  // migration (falling back to discovery only when the ID is absent),
  // never silently substitute a different migration.
  const { data, error, loading, updatedAt, stale, refresh } = useDashboard(false, {
    migrationId: id || null,
  });

  if (loading && !data) return <Loading label="migration detail" />;
  if (!data || !data.summary) {
    return (
      <>
        <ConnectionBanner connected={false} updatedAt={updatedAt} stale={stale} />
        <ErrorState label="Migration detail" error={error || "migration unavailable"} onRetry={refresh} />
      </>
    );
  }
  if (id && data.migrationId && data.migrationId !== id) {
    return (
      <>
        <ConnectionBanner connected={!error} updatedAt={updatedAt} stale={stale} />
        <ErrorState
          label="Migration detail"
          error={`URL migration ${id} did not resolve (loaded ${data.migrationId}). Check the migration ID.`}
          onRetry={refresh}
        />
      </>
    );
  }
  const s = data.summary;

  return (
    <>
      <div className="page-head">
        <h2>Migration detail</h2>
        <p className="muted">
          Migration <code>{id}</code> · workload <code>{s.workload_id}</code>
        </p>
      </div>
      <ConnectionBanner connected={!error} updatedAt={updatedAt} stale={stale} />

      <SectionCard title="Lifecycle" subtitle="Completed only when the API reports it." testId="migration-lifecycle">
        {s.lifecycle?.stages ? (
          <LifecycleTimeline stages={s.lifecycle.stages} />
        ) : (
          <EmptyState label="lifecycle" />
        )}
      </SectionCard>

      <SectionCard
        title="Cutover state machine"
        subtitle="9 stages. Current stage highlighted; completion requires API evidence."
        testId="migration-cutover"
      >
        <MetricRow label="Cutover status" badge={s.cutover?.status || "UNKNOWN"} />
        <MetricRow label="Current stage" badge={s.cutover?.current_stage || "UNKNOWN"} />
        {s.cutover?.stages ? <CutoverTimeline stages={s.cutover.stages} /> : <EmptyState label="cutover stages" />}
      </SectionCard>

      <SectionCard title="Audit timeline" subtitle="Stage, status, timestamp, request, actor, policy, approval, CDC." testId="migration-audit">
        {data.audit.length === 0 ? (
          <EmptyState label="audit entries" hint="No audit entries for this migration yet." />
        ) : (
          <table className="data">
            <thead>
              <tr>
                <th>Stage / action</th>
                <th>Status</th>
                <th>Timestamp</th>
                <th>Request ID</th>
                <th>Actor</th>
                <th>Policy</th>
                <th>Approval</th>
              </tr>
            </thead>
            <tbody>
              {data.audit.map((a, i) => (
                <tr key={`${a.request_id}-${i}`} data-testid={`audit-row-${a.action}`}>
                  <td><code>{a.action}</code></td>
                  <td><StatusBadge value={a.result} /></td>
                  <td>{a.created_at || "—"}</td>
                  <td><code className="wrap">{a.request_id || "—"}</code></td>
                  <td>{a.actor_id || "—"}</td>
                  <td>{a.policy_decision ? <StatusBadge value={a.policy_decision} /> : "—"}</td>
                  <td>{a.approval_id ? <code className="wrap">{a.approval_id}</code> : "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </SectionCard>

      <AiReviewPanel migrationId={data.migrationId || id} />
    </>
  );
}

export default function MigrationDetailPage() {
  const params = useParams();
  const id = (params?.id as string) || "";
  return (
    <Suspense fallback={<Loading label="migration detail" />}>
      <DetailInner id={id} />
    </Suspense>
  );
}
