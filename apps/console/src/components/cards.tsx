import type { ReactNode } from "react";
import StatusBadge from "./StatusBadge";

export function SectionCard({
  title,
  subtitle,
  children,
  testId,
}: {
  title: string;
  subtitle?: string;
  children: ReactNode;
  testId?: string;
}) {
  return (
    <section className="card" data-testid={testId || title}>
      <header className="card-head">
        <h2>{title}</h2>
        {subtitle ? <p className="muted">{subtitle}</p> : null}
      </header>
      <div className="card-body">{children}</div>
    </section>
  );
}

export function MetricRow({
  label,
  value,
  badge,
}: {
  label: string;
  value?: string;
  badge?: string | number | null;
}) {
  return (
    <div className="metric-row">
      <span className="metric-label">{label}</span>
      <span className="metric-value">
        {badge !== undefined ? (
          <StatusBadge value={badge ?? "UNKNOWN"} />
        ) : (
          <code>{value || "UNKNOWN"}</code>
        )}
      </span>
    </div>
  );
}

export function Loading({ label }: { label: string }) {
  return (
    <div role="status" aria-live="polite" className="state-box" data-testid={`loading-${label}`}>
      <span className="spinner" aria-hidden="true" /> Loading {label}…
    </div>
  );
}

export function ErrorState({ label, error, onRetry }: { label: string; error: string; onRetry?: () => void }) {
  return (
    <div role="alert" className="state-box state-error" data-testid={`error-${label}`}>
      <strong>{label} unavailable.</strong>
      <p className="muted">{error}</p>
      <p className="muted">Showing UNKNOWN instead of assumed values. Check the control plane.</p>
      {onRetry ? (
        <button type="button" className="btn" onClick={onRetry}>
          Retry
        </button>
      ) : null}
    </div>
  );
}

export function EmptyState({ label, hint }: { label: string; hint?: string }) {
  return (
    <div className="state-box" data-testid={`empty-${label}`}>
      <strong>No {label} yet.</strong>
      {hint ? <p className="muted">{hint}</p> : null}
    </div>
  );
}

export function ConnectionBanner({
  connected,
  updatedAt,
  stale,
}: {
  connected: boolean;
  updatedAt: number | null;
  stale: boolean;
}) {
  if (connected && !stale) return null;
  return (
    <div
      role="alert"
      className="banner banner-warn"
      data-testid="connection-banner"
    >
      {!connected
        ? "Control plane disconnected. Values show UNKNOWN; no success is implied."
        : "Data is stale (last update over 30s ago). Values may be outdated."}
      {updatedAt ? (
        <span className="muted"> Last update: {new Date(updatedAt).toISOString()}</span>
      ) : null}
    </div>
  );
}
