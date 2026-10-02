// Target migration plan layer (planning ONLY).
//
// Canonical Workload + Compatibility Report -> TargetMigrationPlan: a
// deterministic, explainable description of what the Azure target must
// contain. The planner CONSUMES compatibility verdicts; it never re-derives
// them and never invents readiness: conditional stays conditional, unknown
// never implies readiness, block yields no executable provisioning action.
//
// The plan is NOT authorization: it provisions nothing, approves nothing,
// and changes no lifecycle state. Every future mutation still crosses
// authorization -> policy -> approval -> workflow -> adapter -> verification.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// PlannerVersion identifies this rule set and output contract; recorded on
// every plan. Bumped to v2 when desired configurations became machine-typed
// (fidelity audit) so stale v1 plans can never silently serve as desired
// state: postDrift rejects plans from older planner versions explicitly.
const PlannerVersion = "planner-v2"

// Provisioning modes: auto (safe to provision later, subject to policy),
// gated (provision only after the validation gate passes), describe-only
// (unresolved: described, must not imply readiness), blocked (no executable
// action for this requirement).
const (
	ModeAuto         = "auto"
	ModeGated        = "gated"
	ModeDescribeOnly = "describe-only"
	ModeBlocked      = "blocked"
)

// PlanComponent is one traceable target element.
type PlanComponent struct {
	Key                  string         `json:"key"`
	LogicalRequirement   string         `json:"logical_requirement"`
	TargetCapability     string         `json:"target_capability"`
	TargetService        string         `json:"target_service"`
	DesiredConfiguration map[string]any `json:"desired_configuration"`
	Dependencies         []string       `json:"dependencies"`
	SecurityRequirements []string       `json:"security_requirements"`
	Evidence             []EvidenceRef  `json:"evidence"`
	CompatibilityStatus  string         `json:"compatibility_status"`
	Assumptions          []string       `json:"assumptions"`
	Blockers             []string       `json:"blockers"`
	ValidationGate       string         `json:"validation_gate"`
	ProvisioningMode     string         `json:"provisioning_mode"`
}

// TargetMigrationPlan is the deterministic desired-target-state document.
type TargetMigrationPlan struct {
	ID                           string          `json:"id"`
	WorkloadID                   string          `json:"workload_id"`
	SourceProvider               string          `json:"source_provider"`
	TargetProvider               string          `json:"target_provider"`
	CanonicalSpecVersion         int             `json:"canonical_spec_version"`
	CompatibilityReportID        string          `json:"compatibility_report_id"`
	CompatibilityRegistryVersion int             `json:"compatibility_registry_version"`
	PlannerVersion               string          `json:"planner_version"`
	OverallStatus                string          `json:"overall_status"`
	Components                   []PlanComponent `json:"components"`
	Dependencies                 []string        `json:"dependencies"`
	Assumptions                  []string        `json:"assumptions"`
	Blockers                     []string        `json:"blockers"`
	ValidationGates              []string        `json:"validation_gates"`
}

// componentOrder is the fixed output order (deterministic).
var componentOrder = []string{
	"compute", "database", "cache", "object-storage", "queue",
	"identity", "network", "routing", "observability", "security", "data-movement",
}

// checkPrefix maps each component to the compatibility-check requirement
// prefix it consumes. Statuses are taken from the report, never recomputed.
var checkPrefix = map[string]string{
	"compute":        "compute:",
	"database":       "database: postgresql",
	"database-net":   "database: private",
	"network":        "network:",
	"object-storage": "object storage:",
	"queue":          "queue:",
	"identity":       "identity:",
	"routing":        "routing:",
	"observability":  "observability:",
}

func findCheck(rep CompatReport, prefix string) *Check {
	for i := range rep.Checks {
		if len(rep.Checks[i].Requirement) >= len(prefix) &&
			rep.Checks[i].Requirement[:len(prefix)] == prefix {
			return &rep.Checks[i]
		}
	}
	return nil
}

// modeFor maps a consumed status to its provisioning mode.
func modeFor(status string) string {
	switch status {
	case "pass":
		return ModeAuto
	case "conditional":
		return ModeGated
	case "unknown":
		return ModeDescribeOnly
	default:
		return ModeBlocked
	}
}

// worse returns the more severe of two statuses (block > unknown > conditional > pass).
func worse(a, b string) string {
	rank := map[string]int{"pass": 0, "conditional": 1, "unknown": 2, "block": 3}
	ra, oka := rank[a]
	rb, okb := rank[b]
	if !oka {
		ra = 2
	}
	if !okb {
		rb = 2
	}
	if ra >= rb {
		return a
	}
	return b
}

// gateText is the explicit prerequisite carried for non-pass components.
func gateText(key, requirement, status string) string {
	return fmt.Sprintf("component %q is %s: %s — validate before provisioning; cutover gate stays closed until resolved", key, status, requirement)
}

// blockerText marks a blocked requirement with no executable action.
func blockerText(key, requirement string) string {
	return fmt.Sprintf("component %q blocked: %s — no provisioning action emitted", key, requirement)
}

// obsBool reads a registry constraint boolean for desired state.
func obsBool(reg *Registry, capID, field string, def bool) bool {
	if cap, ok := reg.byID[capID]; ok && cap != nil {
		if v, ok := cap.Constraints[field].(bool); ok {
			return v
		}
	}
	return def
}

// mergeComponent consumes two compat checks (e.g. engine capability plus
// private-networking posture) into one component. Status is the worse of the
// two; evidence and assumptions union. A missing check degrades to unknown.
func mergeComponent(key, requirement, service string, cfg map[string]any, deps, sec []string, a, b *Check) PlanComponent {
	if a == nil && b == nil {
		return newComponent(key, requirement, service, cfg, deps, sec, nil)
	}
	status := "pass"
	evidence := []EvidenceRef{}
	assumptions := []string{}
	target := ""
	seenTarget := map[string]bool{}
	for _, chk := range []*Check{a, b} {
		if chk == nil {
			status = worse(status, "unknown")
			assumptions = append(assumptions, "one contributing compatibility check is absent")
			continue
		}
		status = worse(status, chk.Status)
		// Deduplicate identical capabilities: one canonical identifier with
		// the merged evidence of all contributing checks. Distinct
		// capabilities remain visibly joined.
		if !seenTarget[chk.TargetCapability] {
			seenTarget[chk.TargetCapability] = true
			if target == "" {
				target = chk.TargetCapability
			} else {
				target = target + " + " + chk.TargetCapability
			}
		}
		evidence = append(evidence, chk.Evidence...)
		assumptions = append(assumptions, chk.Assumptions...)
	}
	merged := &Check{Requirement: requirement, TargetCapability: target,
		Status: status, Evidence: evidence, Assumptions: assumptions}
	return newComponent(key, requirement, service, cfg, deps, sec, merged)
}

// newComponent consumes one compat check into a planned component.
// Unknown checks (absent from the report) degrade to unknown, never to pass.
func newComponent(key, requirement, service string, cfg map[string]any, deps, sec []string, chk *Check) PlanComponent {
	c := PlanComponent{
		Key: key, LogicalRequirement: requirement, TargetService: service,
		DesiredConfiguration: cfg, Dependencies: deps, SecurityRequirements: sec,
		Evidence: []EvidenceRef{}, Assumptions: []string{}, Blockers: []string{},
	}
	if deps == nil {
		c.Dependencies = []string{}
	}
	if sec == nil {
		c.SecurityRequirements = []string{}
	}
	if chk == nil {
		c.CompatibilityStatus = "unknown"
		c.TargetCapability = "unresolved"
		c.Assumptions = []string{"no compatibility check found for this requirement"}
		c.ValidationGate = gateText(key, requirement, "unknown")
		c.ProvisioningMode = ModeDescribeOnly
		return c
	}
	c.TargetCapability = chk.TargetCapability
	c.CompatibilityStatus = chk.Status
	c.Evidence = append([]EvidenceRef{}, chk.Evidence...)
	c.Assumptions = append([]string{}, chk.Assumptions...)
	c.ProvisioningMode = modeFor(chk.Status)
	switch chk.Status {
	case "conditional", "unknown":
		c.ValidationGate = gateText(key, chk.Requirement, chk.Status)
	case "block":
		c.Blockers = []string{blockerText(key, chk.Requirement)}
		c.DesiredConfiguration = map[string]any{} // no executable action
	}
	return c
}

// BuildPlan derives the target plan from a schema-valid spec, its
// compatibility report, and the registry (used for service/config details
// only — verdicts come from rep.Checks). Pure function: no clock, no
// network, fixed ordering.
func BuildPlan(spec map[string]any, rep CompatReport, reg *Registry) TargetMigrationPlan {
	compute, _ := strMap(spec["compute"])
	db, _ := strMap(spec["database"])
	objSpec, _ := strMap(spec["object_storage"])
	queueSpec, _ := strMap(spec["queue"])
	reqSpec, _ := strMap(spec["requirements"])
	privPlane, _ := reqSpec["private_data_plane"].(bool)
	objRequired, _ := objSpec["required"].(bool)
	// versioning_required is the USER's desired-state requirement (Blob
	// versioning must be enabled when true). It is distinct from the
	// registry's provider_versioning_required constraint, which states that
	// provider version IDs are excluded from cross-cloud identity.
		objVersioning, _ := objSpec["versioning_required"].(bool)
	queueDelivery, _ := queueSpec["delivery_semantics"].(string)
	queueDupSafe, _ := queueSpec["duplicate_safe_consumer"].(bool)
	rpoSeconds := intOf(reqSpec["rpo_seconds"], 30)
	rtoSeconds := intOf(reqSpec["rto_seconds"], 900)
	azCount, azStated := num(reqSpec["availability_zones"])
	replicas, _ := num(compute["replicas"])
	major, _ := db["major_version"].(string)
	storage, _ := num(db["storage_gib"])
	exts := []string{}
	if raw, ok := db["extensions"].([]any); ok {
		for _, e := range raw {
			if s, ok := e.(string); ok {
				exts = append(exts, s)
			}
		}
	}
	minZones := 0
	if cap, ok := reg.byID["kubernetes.multi_zone"]; ok && cap != nil {
		if mz, ok := num(cap.Constraints["min_zones"]); ok {
			minZones = int(mz)
		}
	}
	computeCfg := map[string]any{"orchestrator": "kubernetes", "replicas": int(replicas),
		"cpu_millicores": intOf(compute["cpu_millicores"], 0),
		"memory_mib":     intOf(compute["memory_mib"], 0),
		"min_zones": minZones, "scaling": "replicas from canonical spec; autoscaling defined at provisioning"}
	if azStated {
		// Optional requirement: echoed only when stated, never invented.
		computeCfg["availability_zones"] = int(azCount)
	}

	components := []PlanComponent{
		newComponent("compute", "kubernetes replicas must schedule on target AKS",
			"aks (azure)",
			computeCfg,
			[]string{"network", "identity"},
			[]string{"least-privilege workload identity", "no privileged containers"},
			findCheck(rep, checkPrefix["compute"])),
		mergeComponent("database",
			"postgresql engine/version/extensions with private networking on target",
			"azure-database-for-postgresql (azure)",
			map[string]any{"engine": "postgresql", "major_version": major,
				"storage_gib": int(storage), "extensions": exts,
				"private_networking": privPlane, "publicly_accessible": false,
				"backup": "point-in-time-restore capability required; verified in rehearsal",
				"cdc_prerequisite": "logical replication enabled (Phase 7)"},
			[]string{"network", "identity"},
			[]string{"private endpoint only; no public access", "provider-native encryption at rest", "TLS in transit"},
			findCheck(rep, checkPrefix["database"]),
			findCheck(rep, checkPrefix["database-net"])),
		newComponent("cache", "cache-only Redis-compatible target with lazy warming",
			"azure-cache (azure)",
			map[string]any{"authoritative": false, "warm_strategy": "lazy",
				"ttl_seconds": 300, "key_format": "product:{id}"},
			[]string{"compute"},
			[]string{"non-authoritative by design; loss is a performance event, not data loss"},
			findCheck(rep, checkPrefix["compute"])),
		newComponent("object-storage", "blob target with logical identity and reconciliation",
			"blob-storage (azure)",
			map[string]any{"required": objRequired,
				"versioning_required": objVersioning,
				"version_id_identity": false,
				"identity_model": "logical-key-plus-sha256",
				"reconciliation": "manifest key+hash compare"},
			[]string{"identity"},
			[]string{"private endpoint preferred", "provider-native encryption at rest"},
			findCheck(rep, checkPrefix["object-storage"])),
		newComponent("queue", "at-least-once queue with duplicate-safe consumer",
			"service-bus (azure)",
			map[string]any{"delivery": queueDelivery, "duplicate_safe_consumer": queueDupSafe,
				"ordering": "no global ordering assumed"},
			[]string{"compute", "identity"},
			[]string{"least-privilege sender/receiver roles"},
			findCheck(rep, checkPrefix["queue"])),
		newComponent("identity", "least-privilege workload identity without long-lived credentials",
			"entra-workload-id (azure)",
			map[string]any{"least_privilege": true, "federation": "oidc-or-managed-identity",
				"long_lived_credentials": false},
			[]string{},
			[]string{"no static keys; rotation via federation trust"},
			findCheck(rep, checkPrefix["identity"])),
		newComponent("network", "private VNet connectivity, isolation, routing",
			"vnet (azure)",
			map[string]any{"private_data_plane": privPlane,
				"isolation": "default-deny",
				"public_database_endpoints": false,
				"subnets": "workload, data, and private-endpoint subnets defined at provisioning"},
			[]string{},
			[]string{"no public database endpoints", "egress restricted for control-plane"},
			findCheck(rep, checkPrefix["network"])),
		newComponent("routing", "weighted read-only canary behind one hostname",
			"front-door-standard (azure)",
			map[string]any{"origins": []string{"aws", "azure"},
				"canary_stages": []int{0, 1, 5, 25, 50, 100},
				"note": "planning a router does not perform cutover"},
			[]string{"network"},
			[]string{"HTTPS origins only"},
			findCheck(rep, checkPrefix["routing"])),
		newComponent("observability", "metrics/logs/traces with run correlation",
			"azure-monitor (azure)",
			map[string]any{"correlation": []string{"run_id", "workload_id", "trace_id"},
				"metrics": obsBool(reg, "observability.metrics", "metrics", true),
				"logs":    obsBool(reg, "observability.metrics", "logs", true),
				"traces":  obsBool(reg, "observability.metrics", "traces_via_otel", true),
				"dashboards": "control-plane, migration, replication, cutover, policy, agent"},
			[]string{"compute"},
			[]string{"no credentials or unnecessary PII in telemetry"},
			findCheck(rep, checkPrefix["observability"])),
	}

	// Carry the workload's versioning requirement as an explicit provisioning
	// assumption: Blob versioning must be enabled when versioning_required is
	// true, while provider version IDs stay out of cross-cloud identity.
	for i := range components {
		if components[i].Key == "object-storage" && objVersioning {
			components[i].Assumptions = append(components[i].Assumptions,
				"Blob versioning must be enabled at provisioning (workload versioning_required=true)")
		}
	}

	// security: aggregate of identity + network (aggregation, not verdict invention).
	secStatus := worse(statusOf(components[5]), statusOf(components[6]))
	secEvidence := append(append([]EvidenceRef{}, components[5].Evidence...), components[6].Evidence...)
	secAssume := append(append([]string{}, components[5].Assumptions...), components[6].Assumptions...)
	secComp := PlanComponent{
		Key: "security", LogicalRequirement: "least privilege, encryption, network policy, private endpoints, audit",
		TargetCapability: "identity + network posture", TargetService: "entra-workload-id + vnet (azure)",
		DesiredConfiguration: map[string]any{"managed_identity": true,
			"encryption_at_rest": true, "tls": true,
			"network_policy": "default-deny", "private_endpoints": true,
			"audit_export": true,
			"long_lived_credentials": false},
		Dependencies: []string{"identity", "network"},
		SecurityRequirements: []string{"no long-lived credentials", "no public data endpoints",
			"provider-native encryption at rest; TLS 1.2+ in transit",
			"append-only audit export for high-value actions",
			"explicit control-plane/workload communication paths"},
		Evidence: secEvidence, CompatibilityStatus: secStatus, Assumptions: secAssume,
		Blockers: []string{}, ProvisioningMode: modeFor(secStatus),
	}
	if secStatus == "conditional" || secStatus == "unknown" {
		secComp.ValidationGate = gateText("security", "aggregate identity+network posture", secStatus)
	}
	if secStatus == "block" {
		secComp.Blockers = []string{blockerText("security", "aggregate identity+network posture")}
		secComp.DesiredConfiguration = map[string]any{}
	}
	components = append(components, secComp)

	// data-movement: aggregate of database + object-storage + queue, capped at
	// gated (describes prerequisites; executes only via Phase 7-9 workflows).
	dmStatus := worse(worse(statusOf(components[1]), statusOf(components[3])), statusOf(components[4]))
	dmEvidence := append(append(append([]EvidenceRef{}, components[1].Evidence...), components[3].Evidence...), components[4].Evidence...)
	dmAssume := append(append(append([]string{}, components[1].Assumptions...), components[3].Assumptions...), components[4].Assumptions...)
	dmAssume = append(dmAssume, "data movement executes only via Phase 7-9 workflows, never from the plan alone")
	dmMode := modeFor(dmStatus)
	if dmMode == ModeAuto {
		dmMode = ModeGated
	}
	dmComp := PlanComponent{
		Key: "data-movement",
		LogicalRequirement: "ordered state migration: snapshot+CDC catch-up, object reconcile, queue drain, cache rebuild",
		TargetCapability: "database + object + queue posture", TargetService: "derived (azure)",
		DesiredConfiguration: map[string]any{"postgres_cdc": fmt.Sprintf("snapshot + catch-up to lag<=%ds", rpoSeconds),
			"rpo_seconds": rpoSeconds, "rto_seconds": rtoSeconds,
			"object": "manifest reconcile", "queue": "pause/drain/resume",
			"cache": "rebuild lazily", "order": []string{"database", "object-storage", "queue", "cache"}},
		Dependencies: []string{"database", "object-storage", "queue"},
		SecurityRequirements: []string{"replication uses scoped workload identity"},
		Evidence: dmEvidence, CompatibilityStatus: dmStatus, Assumptions: dmAssume,
		Blockers: []string{}, ProvisioningMode: dmMode,
	}
	if dmStatus == "conditional" || dmStatus == "unknown" {
		dmComp.ValidationGate = gateText("data-movement", "ordered state migration prerequisites", dmStatus)
	}
	if dmStatus == "block" {
		dmComp.Blockers = []string{blockerText("data-movement", "ordered state migration prerequisites")}
		dmComp.DesiredConfiguration = map[string]any{}
	}
	components = append(components, dmComp)

	// Global ordered aggregation in component order.
	deps, assumptions, blockers, gates := []string{}, []string{}, []string{}, []string{}
	seenDep := map[string]bool{}
	for _, c := range components {
		for _, d := range c.Dependencies {
			edge := c.Key + " requires " + d
			if !seenDep[edge] {
				seenDep[edge] = true
				deps = append(deps, edge)
			}
		}
		assumptions = append(assumptions, c.Assumptions...)
		blockers = append(blockers, c.Blockers...)
		if c.ValidationGate != "" {
			gates = append(gates, c.ValidationGate)
		}
	}

	specRaw, _ := json.Marshal(spec)
	specHash, _ := canonicalHash(specRaw)
	return TargetMigrationPlan{
		ID:                           PlanID(rep.WorkloadID, rep.ID, reg.Version, specHash),
		WorkloadID:                   rep.WorkloadID,
		SourceProvider:               "aws",
		TargetProvider:               rep.TargetProvider,
		CanonicalSpecVersion:         intOf(spec["schema_version"], 1),
		CompatibilityReportID:        rep.ID,
		CompatibilityRegistryVersion: reg.Version,
		PlannerVersion:               PlannerVersion,
		OverallStatus:                rep.Status,
		Components:                   components,
		Dependencies:                 deps,
		Assumptions:                  assumptions,
		Blockers:                     blockers,
		ValidationGates:              gates,
	}
}

func statusOf(c PlanComponent) string { return c.CompatibilityStatus }

func intOf(v any, def int) int {
	if f, ok := num(v); ok {
		return int(f)
	}
	return def
}

// PlanID deterministically identifies a plan for idempotent persistence.
func PlanID(workloadID, reportID string, regVersion int, specHash string) string {
	hexStr := sha256HexCompat([]byte(workloadID + "|" + reportID + "|" + itoa(regVersion) + "|" + PlannerVersion + "|" + specHash))
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexStr[0:8], hexStr[8:12], hexStr[12:16], hexStr[16:20], hexStr[20:32])
}

// planBytes marshals a plan for HTTP responses and idempotency storage.
func planBytes(plan TargetMigrationPlan) ([]byte, error) {
	return json.Marshal(plan)
}

// postPlan implements POST /v1/workloads/{id}/plan.
// Planning ONLY: consumes the latest compatibility report for the CURRENT
// spec. A missing report, or a report evaluated against a different spec
// (stale: deterministic ID mismatch), fails explicitly with 409 — the plan
// never silently regenerates or assumes compatibility. Planning creates no
// audit events (analysis, not a mutation) and changes no lifecycle state.
func postPlan(w http.ResponseWriter, r *http.Request, workloadID string) {
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
	var in struct {
		TargetProvider string `json:"target_provider"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || in.TargetProvider == "" {
		writeErr(w, rid, "VALIDATION_FAILED", "target_provider required", 400)
		return
	}
	if in.TargetProvider != "azure" {
		writeErr(w, rid, "VALIDATION_FAILED", "target_provider must be azure", 400)
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
	spec, ok := wl["canonical_spec"].(map[string]any)
	if !ok {
		writeErr(w, rid, "VALIDATION_FAILED", "canonical_spec unavailable for workload", 400)
		return
	}
	if failures := ValidateCanonical(spec); failures != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "VALIDATION_FAILED", "message": "canonical workload does not conform to schema",
			"request_id": rid, "details": map[string]any{"failures": failures}}})
		return
	}
	reg, err := EmbeddedRegistry()
	if err != nil {
		writeErr(w, rid, "INTERNAL_ERROR", "capability registry unavailable", 500)
		return
	}
	specRaw, _ := json.Marshal(spec)
	specHash, _ := canonicalHash(specRaw)
	rep, ok := store.GetLatestCompatReport(workloadID)
	if !ok || rep.ID != ReportID(workloadID, reg.Version, specHash) ||
		rep.RegistryVersion != reg.Version || rep.TargetProvider != in.TargetProvider {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "COMPATIBILITY_STALE",
			"message": "no current compatibility report for this spec; evaluate compatibility first",
			"request_id": rid, "details": map[string]any{}}})
		return
	}
	plan := BuildPlan(spec, rep, reg)
	if err := store.SaveTargetPlan(plan); err != nil {
		writeErr(w, rid, "INTERNAL_ERROR", "plan persist failed", 500)
		return
	}
	resp, _ := planBytes(plan)
	store.SaveIdem(key, hash, resp, 202)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(202)
	_, _ = w.Write(resp)
}

// getPlanLatest implements GET /v1/workloads/{id}/plan (latest plan).
func getPlanLatest(w http.ResponseWriter, r *http.Request, workloadID string) {
	rid := reqID(r)
	if _, ok := store.GetWorkload(workloadID); !ok {
		writeErr(w, rid, "NOT_FOUND", "workload not found", 404)
		return
	}
	plan, ok := store.GetLatestTargetPlan(workloadID)
	if !ok {
		writeErr(w, rid, "NOT_FOUND", "no target plan yet for workload", 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(plan)
}
