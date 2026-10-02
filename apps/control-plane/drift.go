// Drift reconciliation evaluator (FR-017, detection ONLY).
//
// Desired target state (TargetMigrationPlan) + observed target snapshot ->
// DriftReport. Never mutates, never remediates, never authorizes: findings
// are evidence for a future cutover gate. Desired and observed inputs are
// never modified and never merged into each other.
//
// No AI in the verdict path. No cloud calls. Pure function of
// (plan, snapshot): deterministic IDs, deterministic ordering.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// DriftEvaluatorVersion identifies this rule set.
const DriftEvaluatorVersion = "drift-v1"

// SnapshotVersion is the only observed-snapshot version this evaluator reads.
const SnapshotVersion = 1

// ObservedSnapshot is the provider-neutral observed target state.
// observed_at is informational only and excluded from identity.
type ObservedSnapshot struct {
	SnapshotVersion int                       `json:"snapshot_version"`
	ObservedAt      string                    `json:"observed_at"`
	Components      map[string]map[string]any `json:"components"`
}

// DriftFinding is one structured difference.
type DriftFinding struct {
	FindingID     string       `json:"finding_id"`
	Severity      string       `json:"severity"`
	Component     string       `json:"component"`
	Path          string       `json:"path"`
	ExpectedValue any          `json:"expected_value"`
	ObservedValue any          `json:"observed_value"`
	Reason        string       `json:"reason"`
	Evidence      []EvidenceRef `json:"evidence"`
	RemediationHint string     `json:"remediation_hint"`
}

// DriftReport is one evaluation (API shape mirrors DOMAIN_MODEL DriftReport:
// severity/status/references/diff, plus gate and findings for machines).
type DriftReport struct {
	ID                string         `json:"id"`
	WorkloadID        string         `json:"workload_id"`
	Severity          string         `json:"severity"`
	Status            string         `json:"status"`
	DesiredReference  string         `json:"desired_reference"`
	ObservedReference string         `json:"observed_reference"`
	DriftGate         string         `json:"drift_gate"`
	Findings          []DriftFinding `json:"findings"`
	PlanVersion       string         `json:"planner_version"`
	RegistryVersion   int            `json:"registry_version"`
	SnapshotVersion   int            `json:"snapshot_version"`
	EvaluatorVersion  string         `json:"evaluator_version"`
}

// SnapshotID deterministically identifies a snapshot from its components
// (observed_at excluded: the same observed state at two times is the same
// state for drift purposes).
func SnapshotID(comps map[string]map[string]any) (string, error) {
	b, err := json.Marshal(comps)
	if err != nil {
		return "", err
	}
	h, err := canonicalHash(b)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}

// DriftReportID deterministically identifies an evaluation.
func DriftReportID(workloadID, planID, snapshotID string) string {
	hexStr := sha256HexCompat([]byte(workloadID + "|" + planID + "|" + snapshotID + "|" + DriftEvaluatorVersion))
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexStr[0:8], hexStr[8:12], hexStr[12:16], hexStr[16:20], hexStr[20:32])
}

// redact replaces values at secret-looking paths. Never persist or emit
// passwords, keys, tokens, connection strings, or private keys.
func redact(path string, v any) any {
	lp := strings.ToLower(path)
	for _, s := range []string{"password", "secret", "token", "api_key", "apikey", "connection_string", "private_key", "passwd", "credential"} {
		if strings.Contains(lp, s) {
			return "[REDACTED]"
		}
	}
	return v
}

// jsonEqual compares values by canonical JSON encoding.
func jsonEqual(a, b any) bool {
	ja, err := json.Marshal(a)
	if err != nil {
		return false
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	var va, vb any
	if err := json.Unmarshal(ja, &va); err != nil {
		return false
	}
	if err := json.Unmarshal(jb, &vb); err != nil {
		return false
	}
	ra, _ := json.Marshal(va)
	rb, _ := json.Marshal(vb)
	return string(ra) == string(rb)
}

type findingBuilder struct {
	component string
	evidence  []EvidenceRef
	out       *[]DriftFinding
}

func (f *findingBuilder) add(severity, path string, expected, observed any, reason, hint string) {
	*f.out = append(*f.out, DriftFinding{
		Component: f.component, Severity: severity, Path: path,
		ExpectedValue: redact(path, expected), ObservedValue: redact(path, observed),
		Reason: reason, Evidence: append([]EvidenceRef{}, f.evidence...),
		RemediationHint: hint,
	})
}

const noAutoFix = "v1 performs no automatic correction; reconcile the target to the plan and re-evaluate"

// compareField emits a mismatch finding when desired and observed differ.
func (f *findingBuilder) compareField(desired, observed map[string]any, field, severity, reason string) {
	exp, hasExp := desired[field]
	obs, hasObs := observed[field]
	if !hasExp {
		return // nothing desired: absence of expectation is not drift
	}
	if !hasObs || !jsonEqual(exp, obs) {
		f.add(severity, f.component+"."+field, exp, obs,
			reason+fmt.Sprintf(" (desired=%v observed=%v)", renderShort(exp), renderShort(obs)),
			"reconcile "+f.component+"."+field+" to the desired value; "+noAutoFix)
	}
}

func renderShort(v any) string {
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

// EvaluateDrift compares desired plan components against observed snapshot
// components. Pure: inputs untouched, fixed component order, sorted findings.
func EvaluateDrift(plan TargetMigrationPlan, snap ObservedSnapshot) DriftReport {
	findings := []DriftFinding{}
	desiredByKey := map[string]PlanComponent{}
	for _, c := range plan.Components {
		desiredByKey[c.Key] = c
	}

	// Fixed evaluation order = plan component order (deterministic).
	for _, comp := range plan.Components {
		fb := &findingBuilder{component: comp.Key, evidence: comp.Evidence, out: &findings}
		obs, present := snap.Components[comp.Key]
		if !present {
			if comp.Key == "data-movement" {
				continue // derived component: covered via constituents + prerequisites
			}
			fb.add("blocking", comp.Key, "present", "absent",
				"required component missing from observed target",
				"provision or register the missing component, then re-evaluate; "+noAutoFix)
			continue
		}
		desired := comp.DesiredConfiguration
		if desired == nil {
			desired = map[string]any{}
		}
		compareComponent(comp.Key, desired, obs, fb)
		// Conditional prerequisites: a gated component requires observed
		// prerequisite_validated == true, else the condition is unsatisfied.
		if comp.ValidationGate != "" {
			if v, _ := obs["prerequisite_validated"].(bool); !v {
				fb.add("blocking", comp.Key+".prerequisite_validated", true, obs["prerequisite_validated"],
					"conditional prerequisite not satisfied: "+comp.ValidationGate,
					"satisfy the validation gate out-of-band, record prerequisite_validated=true, re-evaluate; "+noAutoFix)
			}
		}
	}

	// Unexpected observed components: informational only, never gate-affecting,
	// and never a false PASS (unknown content is recorded, not trusted).
	for key := range snap.Components {
		if _, ok := desiredByKey[key]; !ok && key != "data-movement" {
			fb := &findingBuilder{component: key, out: &findings}
			fb.add("informational", key, "unmodeled", "present",
				"observed component has no desired-state counterpart; ignored for gates",
				"model the component in the plan if it matters, otherwise ignore; "+noAutoFix)
		}
	}

	// Unexpected fields within known components: informational, recorded.
	for _, comp := range plan.Components {
		obs, present := snap.Components[comp.Key]
		if !present {
			continue
		}
		desired := comp.DesiredConfiguration
		extra := []string{}
		for k := range obs {
			if k == "prerequisite_validated" {
				continue
			}
			if _, ok := desired[k]; !ok {
				extra = append(extra, k)
			}
		}
		sort.Strings(extra)
		for _, k := range extra {
			fb := &findingBuilder{component: comp.Key, evidence: comp.Evidence, out: &findings}
			fb.add("informational", comp.Key+"."+k, "unmodeled", obs[k],
				"observed field has no desired value; recorded without affecting gates",
				"add the field to desired state if it matters; "+noAutoFix)
		}
	}

	// Deterministic order: component (plan order), path, severity, reason.
	order := map[string]int{}
	for i, c := range plan.Components {
		order[c.Key] = i
	}
	sort.SliceStable(findings, func(a, b int) bool {
		oa, oka := order[findings[a].Component]
		ob, okb := order[findings[b].Component]
		if oka && okb && oa != ob {
			return oa < ob
		}
		if findings[a].Component != findings[b].Component {
			return findings[a].Component < findings[b].Component
		}
		if findings[a].Path != findings[b].Path {
			return findings[a].Path < findings[b].Path
		}
		return findings[a].Reason < findings[b].Reason
	})
	for i := range findings {
		findings[i].FindingID = driftFindingID(plan.ID, &findings[i])
	}

	gate, severity := aggregateDrift(findings)
	snapID, _ := SnapshotID(snap.Components)
	return DriftReport{
		ID:               DriftReportID(plan.WorkloadID, plan.ID, snapID),
		WorkloadID:       plan.WorkloadID,
		Severity:         severity,
		Status:           "open",
		DesiredReference: plan.ID,
		ObservedReference: snapID,
		DriftGate:        gate,
		Findings:         findings,
		PlanVersion:      plan.PlannerVersion,
		RegistryVersion:  plan.CompatibilityRegistryVersion,
		SnapshotVersion:  SnapshotVersion,
		EvaluatorVersion: DriftEvaluatorVersion,
	}
}

func driftFindingID(planID string, f *DriftFinding) string {
	eb, _ := json.Marshal(f.ExpectedValue)
	ob, _ := json.Marshal(f.ObservedValue)
	hexStr := sha256HexCompat([]byte(planID + "|" + f.Component + "|" + f.Path + "|" + f.Severity + "|" + string(eb) + "|" + string(ob)))
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexStr[0:8], hexStr[8:12], hexStr[12:16], hexStr[16:20], hexStr[20:32])
}

// aggregateDrift: security_critical > blocking > clean. A clean report means
// only "no drift-based blocker detected" — never migration authorization.
func aggregateDrift(findings []DriftFinding) (gate, severity string) {
	gate, severity = "clear", "informational"
	for _, f := range findings {
		switch f.Severity {
		case "security_critical":
			return "security_critical", "security_critical"
		case "blocking":
			gate, severity = "blocking", "blocking"
		}
	}
	return gate, severity
}

// compareComponent applies the per-component field rules. Severity follows
// existing repository rules: security posture violations are
// security_critical; unmet execution prerequisites are blocking;
// non-critical operational differences are informational.
func compareComponent(key string, desired, observed map[string]any, fb *findingBuilder) {
	switch key {
	case "compute":
		fb.compareField(desired, observed, "orchestrator", "blocking", "orchestrator mismatch breaks scheduling")
		fb.compareField(desired, observed, "replicas", "blocking", "replica count mismatch breaks capacity")
		fb.compareField(desired, observed, "cpu_millicores", "blocking", "CPU mismatch breaks node sizing")
		fb.compareField(desired, observed, "memory_mib", "blocking", "memory mismatch breaks node sizing")
		fb.compareField(desired, observed, "availability_zones", "blocking", "zone topology mismatch")
	case "database":
		fb.compareField(desired, observed, "engine", "blocking", "engine mismatch")
		fb.compareField(desired, observed, "major_version", "blocking", "major version mismatch breaks compatibility")
		fb.compareField(desired, observed, "storage_gib", "blocking", "storage mismatch")
		fb.compareField(desired, observed, "extensions", "blocking", "extension set mismatch")
		fb.compareField(desired, observed, "private_networking", "blocking", "private connectivity mismatch")
		if pub, _ := observed["publicly_accessible"].(bool); pub {
			fb.add("security_critical", key+".publicly_accessible", false, true,
				"database publicly exposed",
				"remove public exposure immediately, then re-evaluate; "+noAutoFix)
		}
	case "cache":
		if auth, _ := observed["authoritative"].(bool); auth {
			if exp, _ := desired["authoritative"].(bool); !exp {
				fb.add("blocking", key+".authoritative", false, true,
					"cache observed authoritative while desired is cache-only; risks split-brain writes",
					"restore cache-only behavior with lazy warming; "+noAutoFix)
			}
		}
		fb.compareField(desired, observed, "ttl_seconds", "informational", "TTL tuning difference")
		fb.compareField(desired, observed, "warm_strategy", "informational", "warm strategy difference")
	case "object-storage":
		fb.compareField(desired, observed, "required", "blocking", "object storage requirement mismatch")
		fb.compareField(desired, observed, "versioning_required", "blocking", "versioning mismatch breaks retention safety")
		fb.compareField(desired, observed, "version_id_identity", "blocking", "version-ID identity model mismatch breaks reconciliation")
		fb.compareField(desired, observed, "identity_model", "blocking", "identity/hash model mismatch breaks reconciliation")
	case "queue":
		fb.compareField(desired, observed, "delivery", "blocking", "delivery semantics mismatch")
		if safe, _ := observed["duplicate_safe_consumer"].(bool); !safe {
			if exp, _ := desired["duplicate_safe_consumer"].(bool); exp {
				fb.add("blocking", key+".duplicate_safe_consumer", true, observed["duplicate_safe_consumer"],
					"consumer not duplicate-safe; at-least-once delivery would double-apply",
					"restore duplicate-safe consumer before migration; "+noAutoFix)
			}
		}
	case "identity":
		if ll, _ := observed["long_lived_credentials"].(bool); ll {
			fb.add("security_critical", key+".long_lived_credentials", false, true,
				"long-lived credentials present; violates secret-free workload authentication",
				"revoke static credentials, restore federated identity, then re-evaluate; "+noAutoFix)
		} else {
			fb.compareField(desired, observed, "long_lived_credentials", "security_critical", "credential posture mismatch")
		}
		if lp, _ := observed["least_privilege"].(bool); !lp {
			fb.add("security_critical", key+".least_privilege", true, observed["least_privilege"],
				"permissions expanded beyond least privilege",
				"scope permissions back down, then re-evaluate; "+noAutoFix)
		}
		fb.compareField(desired, observed, "federation", "blocking", "federation mechanism mismatch")
	case "network":
		fb.compareField(desired, observed, "private_data_plane", "blocking", "private data plane mismatch")
		fb.compareField(desired, observed, "isolation", "security_critical", "isolation weakened below default-deny")
		if pub, _ := observed["public_database_endpoints"].(bool); pub {
			fb.add("security_critical", key+".public_database_endpoints", false, true,
				"database endpoints publicly reachable",
				"remove public database endpoints immediately, then re-evaluate; "+noAutoFix)
		}
	case "routing":
		fb.compareField(desired, observed, "origins", "blocking", "origin set mismatch breaks canary")
		fb.compareField(desired, observed, "canary_stages", "blocking", "canary stage mismatch")
	case "observability":
		fb.compareField(desired, observed, "correlation", "informational", "correlation coverage difference")
		fb.compareField(desired, observed, "metrics", "informational", "metrics coverage difference")
		fb.compareField(desired, observed, "logs", "informational", "log coverage difference")
		fb.compareField(desired, observed, "traces", "informational", "trace coverage difference")
	case "security":
		for _, field := range []string{"managed_identity", "encryption_at_rest", "tls", "private_endpoints", "audit_export"} {
			fb.compareField(desired, observed, field, "security_critical", field+" posture mismatch")
		}
		fb.compareField(desired, observed, "network_policy", "security_critical", "network policy weakened")
		fb.compareField(desired, observed, "long_lived_credentials", "security_critical", "credential posture mismatch")
	default:
		// Unknown desired component: compare nothing, record presence.
		fb.add("informational", key, "unmodeled-desired", "present",
			"no comparison rules for this component; recorded without affecting gates",
			"add comparison rules if this component gates migration; "+noAutoFix)
	}
}

// driftReportBytes marshals a report for HTTP responses and idempotency storage.
func driftReportBytes(rep DriftReport) ([]byte, error) {
	if rep.Findings == nil {
		rep.Findings = []DriftFinding{}
	}
	return json.Marshal(rep)
}

// postDrift implements POST /v1/workloads/{id}/drift.
// Detection only: evaluates the latest target plan against the submitted
// observed snapshot. Requires a current plan (missing/stale fails
// explicitly); creates no audit events (analysis, not a mutation) and no
// migration records.
func postDrift(w http.ResponseWriter, r *http.Request, workloadID string) {
	rid := reqID(r)
	actor, ok := requireAuth(w, r)
	if !ok {
		return
	}
	if !requireWorkloadAccess(w, r, actor, workloadID) {
		return
	}
	wl, _ := store.GetWorkload(workloadID)
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 {
		writeErr(w, rid, "VALIDATION_FAILED", "Idempotency-Key required (min 16 chars)", 400)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "unreadable body", 400)
		return
	}
	hash, err := canonicalHash(raw)
	if err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "invalid JSON", 400)
		return
	}
	var snap ObservedSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "invalid JSON", 400)
		return
	}
	if snap.SnapshotVersion != SnapshotVersion {
		writeErr(w, rid, "VALIDATION_FAILED",
			fmt.Sprintf("snapshot_version must be %d", SnapshotVersion), 400)
		return
	}
	if len(snap.Components) == 0 {
		writeErr(w, rid, "VALIDATION_FAILED", "snapshot components required", 400)
		return
	}
	mu := idemLock(key)
	mu.Lock()
	defer mu.Unlock()
	if resp, status, storedHash, found := store.CheckIdem(key); found {
		if storedHash == "" || storedHash == hash {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(resp)
			return
		}
		writeErr(w, rid, "IDEMPOTENCY_CONFLICT", "same Idempotency-Key with different request body", 409)
		return
	}
	plan, ok := store.GetLatestTargetPlan(workloadID)
	if !ok {
		writeErr(w, rid, "NOT_FOUND", "no target plan yet for workload; generate a plan first", 404)
		return
	}
	// Staleness: the plan must derive from the current spec and registry.
	spec, ok := wl["canonical_spec"].(map[string]any)
	if !ok {
		writeErr(w, rid, "VALIDATION_FAILED", "canonical_spec unavailable for workload", 400)
		return
	}
	reg, err := EmbeddedRegistry()
	if err != nil {
		writeErr(w, rid, "INTERNAL_ERROR", "capability registry unavailable", 500)
		return
	}
	specRaw, _ := json.Marshal(spec)
	specHash, _ := canonicalHash(specRaw)
	// Staleness is threefold: the plan must derive from the current spec and
	// registry AND have been built by the current planner version. Anything
	// else fails explicitly instead of comparing against outdated desired
	// state (which surfaces as false "unmodeled" findings).
	if plan.CompatibilityReportID != ReportID(workloadID, reg.Version, specHash) ||
		plan.CompatibilityRegistryVersion != reg.Version ||
		plan.PlannerVersion != PlannerVersion {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "PLAN_STALE",
			"message": "target plan is stale for the current spec; regenerate compatibility and plan first",
			"request_id": rid, "details": map[string]any{}}})
		return
	}
	rep := EvaluateDrift(plan, snap)
	if err := store.SaveDriftReport(rep); err != nil {
		writeErr(w, rid, "INTERNAL_ERROR", "report persist failed", 500)
		return
	}
	resp, _ := driftReportBytes(rep)
	store.SaveIdem(key, hash, resp, 202)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(202)
	_, _ = w.Write(resp)
}

// getDriftList implements GET /v1/workloads/{id}/drift[?status=].
func getDriftList(w http.ResponseWriter, r *http.Request, workloadID string) {
	rid := reqID(r)
	if _, ok := store.GetWorkload(workloadID); !ok {
		writeErr(w, rid, "NOT_FOUND", "workload not found", 404)
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "", "open", "acknowledged", "resolved", "ignored":
	default:
		writeErr(w, rid, "VALIDATION_FAILED", "invalid status filter", 400)
		return
	}
	items := store.GetDriftReports(workloadID, status)
	if items == nil {
		items = []DriftReport{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
}
