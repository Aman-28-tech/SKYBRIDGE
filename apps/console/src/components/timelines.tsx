import type { CutoverStage, LifecycleStage } from "@/lib/types";
import StatusBadge from "./StatusBadge";

function stageClass(status: string): string {
  if (status === "completed") return "stage-done";
  if (status === "current") return "stage-current";
  if (status === "blocked") return "stage-blocked";
  return "stage-pending";
}

export function LifecycleTimeline({ stages }: { stages: LifecycleStage[] }) {
  return (
    <ol className="timeline" data-testid="lifecycle-timeline" aria-label="Migration lifecycle">
      {stages.map((s) => (
        <li key={s.name} className={`stage ${stageClass(s.status)}`} data-testid={`lifecycle-${s.name}`}>
          <span className="stage-dot" aria-hidden="true" />
          <div className="stage-content">
            <div className="stage-title">
              <code>{s.name}</code>
              <StatusBadge value={s.status} />
            </div>
            {s.timestamp ? <div className="muted small">{s.timestamp}</div> : null}
            {s.detail ? <div className="muted small">{s.detail}</div> : null}
          </div>
        </li>
      ))}
    </ol>
  );
}

export function CutoverTimeline({ stages }: { stages: CutoverStage[] }) {
  return (
    <ol className="timeline" data-testid="cutover-timeline" aria-label="Cutover state machine">
      {stages.map((s) => (
        <li key={s.name} className={`stage ${stageClass(s.status)}`} data-testid={`cutover-${s.name}`}>
          <span className="stage-dot" aria-hidden="true" />
          <div className="stage-content">
            <div className="stage-title">
              <code>{s.name}</code>
              <StatusBadge value={s.status} />
            </div>
            <dl className="stage-meta">
              {s.timestamp ? (<><dt>timestamp</dt><dd>{s.timestamp}</dd></>) : null}
              {s.request_id ? (<><dt>request ID</dt><dd><code>{s.request_id}</code></dd></>) : null}
              {s.actor ? (<><dt>actor</dt><dd>{s.actor}</dd></>) : null}
              {s.policy_hash ? (<><dt>policy hash</dt><dd><code className="wrap">{s.policy_hash}</code></dd></>) : null}
              {s.approval_id ? (<><dt>approval ID</dt><dd><code className="wrap">{s.approval_id}</code></dd></>) : null}
              {s.source_lsn ? (<><dt>source LSN</dt><dd><code>{s.source_lsn}</code></dd></>) : null}
              {s.applied_lsn ? (<><dt>applied LSN</dt><dd><code>{s.applied_lsn}</code></dd></>) : null}
              {s.lag_seconds !== null && s.lag_seconds !== undefined ? (<><dt>lag</dt><dd>{String(s.lag_seconds)}s</dd></>) : null}
              {s.detail ? (<><dt>detail</dt><dd>{s.detail}</dd></>) : null}
            </dl>
          </div>
        </li>
      ))}
    </ol>
  );
}
