import { SectionCard } from "@/components/cards";
import StatusBadge from "@/components/StatusBadge";
import { CAPABILITY_MATRIX, EVIDENCE_BOUNDARY } from "@/lib/capability-matrix";

export default function EvidencePage() {
  const proven = CAPABILITY_MATRIX.filter((r) => r.status === "PROVEN");
  const partial = CAPABILITY_MATRIX.filter((r) => r.status === "PARTIALLY PROVEN");
  const deferred = CAPABILITY_MATRIX.filter((r) => r.status === "DEFERRED");

  const group = (title: string, rows: typeof proven, testId: string) => (
    <SectionCard title={title} testId={testId}>
      <table className="data">
        <thead>
          <tr>
            <th>Capability</th>
            <th>Status</th>
            <th>Evidence</th>
            <th>Scope</th>
            <th>Notes</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.capability}>
              <td>{r.capability}</td>
              <td><StatusBadge value={r.status} /></td>
              <td>{r.evidence}</td>
              <td>{r.scope}</td>
              <td className="muted">{r.notes}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </SectionCard>
  );

  return (
    <>
      <div className="page-head">
        <h2>Evidence</h2>
        <p className="muted">Capability matrix (docs/CAPABILITY_MATRIX.md) plus the local-only boundary.</p>
      </div>
      <SectionCard title="Boundary" testId="boundary-panel">
        <ul>
          <li>Local-only boundary: {EVIDENCE_BOUNDARY.localOnly}</li>
          <li>Real AWS status: {EVIDENCE_BOUNDARY.realAws}</li>
          <li>Real Azure status: {EVIDENCE_BOUNDARY.realAzure}</li>
          <li>Terraform Apply status: {EVIDENCE_BOUNDARY.terraformApply}</li>
        </ul>
        <p data-testid="no-mutation-note">
          <strong>{EVIDENCE_BOUNDARY.noMutation}</strong>
        </p>
      </SectionCard>
      {group(`Proven (${proven.length})`, proven, "evidence-proven")}
      {group(`Partially proven (${partial.length})`, partial, "evidence-partial")}
      {group(`Deferred (${deferred.length})`, deferred, "evidence-deferred")}
    </>
  );
}
