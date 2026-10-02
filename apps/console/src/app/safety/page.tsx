"use client";

import { Suspense } from "react";
import { ConnectionBanner, ErrorState, Loading, MetricRow, SectionCard } from "@/components/cards";
import { SafetyPanel } from "@/components/panels";
import { useDashboard } from "@/lib/use-summary";

function Inner() {
  const { data, error, loading, updatedAt, stale, refresh } = useDashboard(true);
  if (loading && !data) return <Loading label="safety" />;
  if (!data || !data.summary) {
    return (
      <>
        <ConnectionBanner connected={false} updatedAt={updatedAt} stale={stale} />
        <ErrorState label="Safety" error={error || "safety unavailable"} onRetry={refresh} />
      </>
    );
  }
  const s = data.summary;
  return (
    <>
      <div className="page-head">
        <h2>Safety</h2>
        <p className="muted">Fail-closed evidence. Blocked operations are highlighted. No mutation controls exist.</p>
      </div>
      <ConnectionBanner connected={!error} updatedAt={updatedAt} stale={stale} />
      <SafetyPanel safety={s.safety || null} />
      <SectionCard title="Rollback" testId="rollback-panel">
        <MetricRow label="Rollback status" badge={s.safety?.rollback || "UNKNOWN"} />
        <p className="muted small">
          v1 offers no rollback button. After Azure becomes authoritative, traffic-only rollback is
          rejected by the control plane (POST_WRITE_ROLLBACK_BLOCKED); recovery is forward-fix.
        </p>
      </SectionCard>
    </>
  );
}

export default function SafetyPage() {
  return (
    <Suspense fallback={<Loading label="safety" />}>
      <Inner />
    </Suspense>
  );
}
