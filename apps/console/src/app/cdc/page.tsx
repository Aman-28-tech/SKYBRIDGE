"use client";

import { Suspense } from "react";
import { ConnectionBanner, ErrorState, Loading, MetricRow, SectionCard } from "@/components/cards";
import { CdcPanel } from "@/components/panels";
import StatusBadge from "@/components/StatusBadge";
import { useDashboard } from "@/lib/use-summary";

function Inner() {
  const { data, error, loading, updatedAt, stale, refresh } = useDashboard(true);
  if (loading && !data) return <Loading label="CDC" />;
  if (!data || !data.summary) {
    return (
      <>
        <ConnectionBanner connected={false} updatedAt={updatedAt} stale={stale} />
        <ErrorState label="CDC" error={error || "CDC unavailable"} onRetry={refresh} />
      </>
    );
  }
  const cdc = data.summary.cdc;
  return (
    <>
      <div className="page-head">
        <h2>CDC</h2>
        <p className="muted">Measured replication state. History is not fabricated.</p>
      </div>
      <ConnectionBanner connected={!error} updatedAt={updatedAt} stale={stale} />
      <CdcPanel cdc={cdc || null} />
      <SectionCard title="Reconciliation detail" testId="recon-panel">
        <MetricRow
          label="Match"
          badge={cdc?.reconciliation_match === true ? "MATCH" : cdc?.reconciliation_match === false ? "MISMATCH" : "UNKNOWN"}
        />
        <MetricRow label="Fingerprint" value={cdc?.reconciliation_fingerprint || "UNKNOWN"} />
        <MetricRow
          label="Within RPO"
          badge={cdc?.within_rpo === true ? "YES" : cdc?.within_rpo === false ? "NO" : "UNKNOWN"}
        />
        <p className="muted small">
          Reconciliation is probe-scoped by design (seed divergence excluded).{" "}
          <StatusBadge value={cdc?.within_rpo === false ? "RPO_BREACH" : "WITHIN_RPO"} />
        </p>
      </SectionCard>
    </>
  );
}

export default function CdcPage() {
  return (
    <Suspense fallback={<Loading label="CDC" />}>
      <Inner />
    </Suspense>
  );
}
