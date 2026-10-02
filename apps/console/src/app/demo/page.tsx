"use client";

import { Suspense } from "react";
import { ConnectionBanner, ErrorState, Loading, SectionCard } from "@/components/cards";
import StatusBadge from "@/components/StatusBadge";
import { AiReviewPanel } from "@/components/AiReview";
import { useDashboard } from "@/lib/use-summary";
import { CUTOVER_ORDER, LIFECYCLE_ORDER } from "@/lib/types";

function stepClass(status: string): string {
  if (status === "completed") return "flow-step done";
  if (status === "current") return "flow-step current";
  return "flow-step";
}

function Inner() {
  const { data, error, loading, updatedAt, stale, refresh } = useDashboard(true);
  if (loading && !data) return <Loading label="live demo" />;
  if (!data || !data.summary) {
    return (
      <>
        <ConnectionBanner connected={false} updatedAt={updatedAt} stale={stale} />
        <ErrorState label="Live demo" error={error || "demo state unavailable"} onRetry={refresh} />
      </>
    );
  }
  const s = data.summary;
  const lcByName = new Map((s.lifecycle?.stages || []).map((x) => [x.name, x.status]));
  const coByName = new Map((s.cutover?.stages || []).map((x) => [x.name, x.status]));

  return (
    <>
      <div className="page-head">
        <h2>Live Demo</h2>
        <p className="muted">
          Presentation view. Reflects real API state (polls every 5s). No animation pretends the
          migration is happening; stages light up only when the API reports them.
          {updatedAt ? ` Last update: ${new Date(updatedAt).toISOString()}` : ""}
        </p>
      </div>
      <ConnectionBanner connected={!error} updatedAt={updatedAt} stale={stale} />

      <SectionCard title="Migration flow" testId="demo-flow">
        <div className="flow" data-testid="demo-lifecycle-flow" aria-label="Migration stages">
          {LIFECYCLE_ORDER.map((name, i) => (
            <span key={name} style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
              <span className={stepClass(lcByName.get(name) || "pending")} data-testid={`demo-lifecycle-${name}`}>
                {name} · {lcByName.get(name) || "pending"}
              </span>
              {i < LIFECYCLE_ORDER.length - 1 ? <span className="flow-arrow" aria-hidden="true">→</span> : null}
            </span>
          ))}
        </div>
        <div className="flow" data-testid="demo-cutover-flow" aria-label="Cutover stages">
          {CUTOVER_ORDER.map((name, i) => (
            <span key={name} style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
              <span className={stepClass(coByName.get(name) || "pending")} data-testid={`demo-cutover-${name}`}>
                {name}
              </span>
              {i < CUTOVER_ORDER.length - 1 ? <span className="flow-arrow" aria-hidden="true">→</span> : null}
            </span>
          ))}
        </div>
      </SectionCard>

      <SectionCard title="Current state" testId="demo-state">
        <p>
          Lifecycle: <StatusBadge value={s.lifecycle?.current || "UNKNOWN"} /> · Cutover:{" "}
          <StatusBadge value={s.cutover?.status || "UNKNOWN"} /> · Authority:{" "}
          <StatusBadge value={s.providers?.authoritative_provider || "UNKNOWN"} />
        </p>
        <p className="muted small">
          Run <code>./scripts/demo-migration.sh</code> with the lab up; this page follows the real
          transitions. Demo IDs: workload <code>{s.workload_id}</code>, migration{" "}
          <code>{s.migration_id}</code>.
        </p>
      </SectionCard>

      <AiReviewPanel migrationId={data.migrationId} compact />
    </>
  );
}

export default function DemoPage() {
  return (
    <Suspense fallback={<Loading label="live demo" />}>
      <Inner />
    </Suspense>
  );
}
