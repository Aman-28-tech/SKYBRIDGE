type Props = {
  value: string | number | null | undefined;
  tone?: "auto" | "neutral";
};

function toneFor(value: string): string {
  const v = value.toUpperCase();
  if (
    ["PASS", "PROVEN", "CUTOVER_COMPLETE", "READY_FOR_CUTOVER", "REHEARSAL_READY", "COMPLETE", "COMPLETED", "SAFE", "AGREE", "ALLOW", "APPROVED", "CLEAR", "MATCH", "WITHIN_RPO", "AZURE", "ACCEPTING"].some(
      (k) => v === k || v.includes(k),
    )
  )
    return "ok";
  if (
    ["FAIL", "BLOCK", "BLOCKED", "DENY", "DENIED", "DETECTED", "DISAGREE", "MISMATCH", "REJECTED", "BREACH", "RPO_BREACH", "SECURITY_CRITICAL", "CUTOVER_BLOCKED", "REHEARSAL_BLOCKED", "NOT_READY"].some(
      (k) => v.includes(k),
    )
  )
    return "bad";
  if (
    ["CONDITIONAL", "UNKNOWN", "PENDING", "CURRENT", "PARTIALLY", "DEFERRED", "NOT_STARTED", "INCONCLUSIVE", "PAUSED", "CUTTING_OVER", "APPROVAL_REQUIRED"].some(
      (k) => v.includes(k),
    )
  )
    return "warn";
  return "neutral";
}

export default function StatusBadge({ value, tone = "auto" }: Props) {
  const text =
    value === null || value === undefined || value === "" ? "UNKNOWN" : String(value);
  const t = tone === "neutral" ? "neutral" : toneFor(text);
  return (
    <span className={`badge badge-${t}`} role="status" aria-label={`status ${text}`}>
      {text}
    </span>
  );
}
