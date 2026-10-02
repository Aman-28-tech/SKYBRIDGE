import type { CdcView, OwnershipView, SafetyView } from "@/lib/types";
import { MetricRow, SectionCard } from "./cards";
import StatusBadge from "./StatusBadge";

export function OwnershipPanel({ ownership }: { ownership: OwnershipView | null }) {
  if (!ownership) {
    return (
      <SectionCard title="Ownership" testId="ownership-panel">
        <p className="muted">UNKNOWN — control plane unreachable.</p>
      </SectionCard>
    );
  }
  return (
    <SectionCard
      title="Ownership"
      subtitle="Single authoritative writer. The non-authoritative provider rejects writes."
      testId="ownership-panel"
    >
      <div className="provider-grid">
        <div className="provider-card" data-testid="provider-aws">
          <h3>AWS</h3>
          <StatusBadge value={ownership.source_writable ? "WRITABLE" : "REJECTING_WRITES"} />
          <p className="muted small">{ownership.source_explanation}</p>
        </div>
        <div className="provider-card" data-testid="provider-azure">
          <h3>AZURE</h3>
          <StatusBadge value={ownership.target_writable ? "WRITABLE" : "REJECTING_WRITES"} />
          <p className="muted small">{ownership.target_explanation}</p>
        </div>
      </div>
      <MetricRow label="Current authority" badge={ownership.current_owner} />
      <MetricRow label="Source writable" badge={ownership.source_writable ? "YES" : "NO"} />
      <MetricRow label="Target writable" badge={ownership.target_writable ? "YES" : "NO"} />
      <MetricRow label="Routing" badge={ownership.routing} />
      <MetricRow label="Split brain" badge={ownership.split_brain} />
    </SectionCard>
  );
}

export function CdcPanel({ cdc }: { cdc: CdcView | null }) {
  if (!cdc) {
    return (
      <SectionCard title="CDC" testId="cdc-panel">
        <p className="muted">UNKNOWN — control plane unreachable.</p>
      </SectionCard>
    );
  }
  const lag = cdc.lag_seconds;
  const rpo = cdc.rpo_seconds ?? 30;
  const pct = lag === null || lag === undefined ? 0 : Math.min(100, Math.round((lag / Math.max(1, rpo)) * 100));
  return (
    <SectionCard
      title="CDC"
      subtitle="Measured positions only. No historical values are fabricated."
      testId="cdc-panel"
    >
      <MetricRow label="Source position" value={cdc.source_lsn || "UNKNOWN"} />
      <MetricRow label="Applied position" value={cdc.applied_lsn || "UNKNOWN"} />
      <MetricRow
        label="Lag"
        badge={lag === null || lag === undefined ? "UNKNOWN" : `${lag}s (RPO ${rpo}s)`}
      />
      <div className="lag-bar" role="img" aria-label={`CDC lag ${lag ?? "unknown"} seconds against RPO ${rpo} seconds`}>
        <div className="lag-fill" style={{ width: `${pct}%` }} />
      </div>
      <MetricRow label="RPO threshold" value={`${rpo}s`} />
      <MetricRow
        label="Captured events"
        value={cdc.events_captured === null || cdc.events_captured === undefined ? "UNKNOWN" : String(cdc.events_captured)}
      />
      <MetricRow
        label="Applied events"
        value={cdc.events_applied === null || cdc.events_applied === undefined ? "UNKNOWN" : String(cdc.events_applied)}
      />
      <MetricRow
        label="Duplicates"
        value={cdc.events_duplicates === null || cdc.events_duplicates === undefined ? "UNKNOWN" : String(cdc.events_duplicates)}
      />
      <MetricRow
        label="Reconciliation"
        badge={cdc.reconciliation_match === true ? "MATCH" : cdc.reconciliation_match === false ? "MISMATCH" : "UNKNOWN"}
      />
    </SectionCard>
  );
}

function blockedTone(v: string): boolean {
  const u = v.toUpperCase();
  return ["BLOCK", "DENIED", "DENY", "MISMATCH", "REJECTED", "BREACH", "FAIL"].some((k) => u.includes(k));
}

export function SafetyPanel({ safety }: { safety: SafetyView | null }) {
  if (!safety) {
    return (
      <SectionCard title="Safety" testId="safety-panel">
        <p className="muted">UNKNOWN — control plane unreachable.</p>
      </SectionCard>
    );
  }
  const rows: Array<[string, string]> = [
    ["Compatibility", safety.compatibility || "UNKNOWN"],
    ["Drift gate", safety.drift_gate || "UNKNOWN"],
    ["Drift severity", safety.drift_severity || "UNKNOWN"],
    ["Policy decision", safety.policy_decision || "UNKNOWN"],
    ["Approval", safety.approval || "NONE"],
    ["Canary", `${safety.canary_verdict || "UNKNOWN"}${safety.canary_stage !== undefined && safety.canary_stage >= 0 ? ` (stage ${safety.canary_stage})` : ""}`],
    ["Quiesce", safety.quiesce || "UNKNOWN"],
    ["Rollback", safety.rollback || "UNKNOWN"],
    ["Split brain", safety.split_brain || "UNKNOWN"],
  ];
  return (
    <SectionCard
      title="Safety"
      subtitle="Blocked operations are highlighted. The console offers no mutation controls."
      testId="safety-panel"
    >
      <ul className="safety-list">
        {rows.map(([label, value]) => (
          <li key={label} className={blockedTone(value) ? "safety-blocked" : ""} data-testid={`safety-${label}`}>
            <span>{label}</span>
            <StatusBadge value={value} />
          </li>
        ))}
      </ul>
      {safety.rollback === "POST_WRITE_ROLLBACK_BLOCKED" ? (
        <p className="blocked-note" role="note">
          Traffic-only rollback to AWS is blocked after Azure became authoritative (no reverse CDC).
          Recovery is forward-fix from Azure state.
        </p>
      ) : null}
    </SectionCard>
  );
}
