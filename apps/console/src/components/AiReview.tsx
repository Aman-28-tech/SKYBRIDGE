"use client";

import { useCallback, useEffect, useState } from "react";
import { apiAiReview } from "@/lib/api";
import { POLL_INTERVAL_MS } from "@/lib/config";
import type { AiReviewEnvelope, AiRiskOrWarning, FetchState } from "@/lib/types";
import { ErrorState, Loading, MetricRow, SectionCard } from "./cards";
import StatusBadge from "./StatusBadge";

export function useAiReview(
  migrationId: string | null,
  poll = true,
): FetchState<AiReviewEnvelope> & { refresh: () => void } {
  const [state, setState] = useState<FetchState<AiReviewEnvelope>>({
    data: null,
    error: null,
    loading: true,
    updatedAt: null,
    stale: false,
  });

  const load = useCallback(async () => {
    if (!migrationId) {
      setState({ data: null, error: "no migration selected", loading: false, updatedAt: Date.now(), stale: false });
      return;
    }
    setState((s) => ({ ...s, loading: s.data === null }));
    const r = await apiAiReview(migrationId);
    setState({
      data: r.data,
      error: r.error,
      loading: false,
      updatedAt: Date.now(),
      stale: false,
    });
  }, [migrationId]);

  useEffect(() => {
    load();
  }, [load]);

  useEffect(() => {
    if (!poll) return;
    const t = setInterval(load, POLL_INTERVAL_MS * 3);
    return () => clearInterval(t);
  }, [poll, load]);

  return { ...state, refresh: load };
}

function CitedList({ items }: { items: AiRiskOrWarning[] }) {
  if (items.length === 0) return <p className="muted small">None reported.</p>;
  return (
    <ul className="safety-list">
      {items.map((item, i) => (
        <li key={i}>
          <span>{item.statement}</span>
        </li>
      ))}
    </ul>
  );
}

function StringList({ items }: { items: string[] }) {
  if (items.length === 0) return <p className="muted small">None.</p>;
  return (
    <ul>
      {items.map((s, i) => (
        <li key={i}><code className="wrap">{s}</code></li>
      ))}
    </ul>
  );
}

export function AiReviewPanel({
  migrationId,
  compact = false,
}: {
  migrationId: string | null;
  compact?: boolean;
}) {
  const { data, error, loading, refresh } = useAiReview(migrationId, !compact);

  if (loading && !data) return <Loading label="AI review" />;
  if (!data || !data.review) {
    return (
      <SectionCard
        title="AI review"
        subtitle="Advisory only — does not authorize migration."
        testId="ai-review-panel"
      >
        <ErrorState
          label="AI review"
          error={error || "AI review unavailable (service unreachable or evidence missing)"}
          onRetry={refresh}
        />
      </SectionCard>
    );
  }

  const review = data.review;
  const a = review.assessment;

  return (
    <SectionCard
      title="AI review"
      subtitle="ADVISORY — DOES NOT AUTHORIZE MIGRATION. Grounded in control-plane evidence; the control plane remains authoritative."
      testId="ai-review-panel"
    >
      <p data-testid="ai-summary">{a.summary}</p>
      <MetricRow label="Confidence" badge={review.confidence} />
      <MetricRow label="Authorization" badge={review.authorization} />
      <MetricRow label="Prompt version" value={data.prompt_version || "UNKNOWN"} />
      <MetricRow label="Model" value={data.model_id || "UNKNOWN"} />
      {!compact ? (
        <>
          <h3 className="muted small">Risks</h3>
          <div data-testid="ai-risks">
            <CitedList items={a.risks} />
          </div>
          <h3 className="muted small">Warnings</h3>
          <div data-testid="ai-warnings">
            <CitedList items={a.warnings} />
          </div>
          <h3 className="muted small">Missing evidence</h3>
          <StringList items={a.missing_evidence} />
          <h3 className="muted small">Recommended checks</h3>
          <StringList items={a.recommended_checks} />
          <h3 className="muted small">Evidence references</h3>
          <StringList items={a.evidence_references} />
          {data.evidence_fingerprint ? (
            <p className="muted small">
              Evidence fingerprint: <code className="wrap">{data.evidence_fingerprint}</code>
            </p>
          ) : null}
        </>
      ) : null}
      {error ? (
        <p className="muted small" data-testid="ai-partial-error">
          Note: {error}
        </p>
      ) : null}
      {data.validation_result && data.validation_result !== "valid" ? (
        <p className="muted small">Validation: {data.validation_result}</p>
      ) : null}
      <p className="muted small">
        AI may recommend, explain, and identify risks.{" "}
        <StatusBadge value="NEVER_BY_AI" /> AI must never authorize or execute.
      </p>
    </SectionCard>
  );
}
