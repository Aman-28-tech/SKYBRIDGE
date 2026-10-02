"use client";

import { Suspense } from "react";
import { ConnectionBanner, ErrorState, Loading } from "@/components/cards";
import { OwnershipPanel } from "@/components/panels";
import { useDashboard } from "@/lib/use-summary";

function Inner() {
  const { data, error, loading, updatedAt, stale, refresh } = useDashboard(true);
  if (loading && !data) return <Loading label="ownership" />;
  if (!data || !data.summary) {
    return (
      <>
        <ConnectionBanner connected={false} updatedAt={updatedAt} stale={stale} />
        <ErrorState label="Ownership" error={error || "ownership unavailable"} onRetry={refresh} />
      </>
    );
  }
  return (
    <>
      <div className="page-head">
        <h2>Ownership</h2>
        <p className="muted">Migration <code>{data.migrationId}</code> · polled from live APIs.</p>
      </div>
      <ConnectionBanner connected={!error} updatedAt={updatedAt} stale={stale} />
      <OwnershipPanel ownership={data.summary.ownership || null} />
    </>
  );
}

export default function OwnershipPage() {
  return (
    <Suspense fallback={<Loading label="ownership" />}>
      <Inner />
    </Suspense>
  );
}
