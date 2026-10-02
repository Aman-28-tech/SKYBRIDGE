// Compatibility evaluation engine (FR-005).
//
// Deterministic, rule-based ANALYSIS — never authorization, never mutation.
// Verdicts derive from workload requirements vs registry entries (with
// evidence); service names are cited as provenance only and never decide.
//
// Registry source: packages/contracts/schemas/capability-registry.example.yaml.
// go:embed cannot leave this module, so a byte-identical copy lives at
// registry/capability-registry.yaml; compat_test.go fails on drift.
// Parsing uses gopkg.in/yaml.v3 (pinned) because the registry's canonical
// format is YAML — no second format was invented.
package main

import (
	_ "embed"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"

	"gopkg.in/yaml.v3"
)

func sha256HexCompat(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

//go:embed registry/capability-registry.yaml
var registryBytes []byte

// EvaluatorVersion identifies this rule set; recorded on every report so
// identical (workload, registry version, evaluator version) inputs always
// yield identical results.
const EvaluatorVersion = "compat-v1"

// EvidenceRef is one provenance entry copied verbatim from the registry.
type EvidenceRef struct {
	Type string `json:"type" yaml:"type"`
	Ref  string `json:"ref" yaml:"ref"`
}

// Capability is one registry entry.
type Capability struct {
	ID          string         `json:"id" yaml:"id"`
	Provider    string         `json:"provider" yaml:"provider"`
	Service     string         `json:"service" yaml:"service"`
	Supported   bool           `json:"supported" yaml:"supported"`
	Constraints map[string]any `json:"constraints" yaml:"constraints"`
	Evidence    []EvidenceRef  `json:"evidence" yaml:"evidence"`
}

// Registry is the parsed capability registry with its version.
type Registry struct {
	Version      int          `json:"version" yaml:"version"`
	Capabilities []Capability `json:"capabilities" yaml:"capabilities"`
	byID         map[string]*Capability
}

// Check is one per-capability finding (matches OpenAPI CompatibilityCheck).
type Check struct {
	Requirement      string       `json:"requirement"`
	TargetCapability string       `json:"target_capability"`
	Status           string       `json:"status"`
	Evidence         []EvidenceRef `json:"evidence"`
	Assumptions      []string     `json:"assumptions"`
}

// CompatReport is one evaluation (matches OpenAPI CompatibilityReport plus
// version traceability; the OpenAPI object permits additional properties).
type CompatReport struct {
	ID               string  `json:"id"`
	WorkloadID       string  `json:"workload_id"`
	TargetProvider   string  `json:"target_provider"`
	Status           string  `json:"status"`
	Checks           []Check `json:"checks"`
	RegistryVersion  int     `json:"registry_version"`
	EvaluatorVersion string  `json:"evaluator_version"`
}

var (
	embeddedRegistry     *Registry
	embeddedRegistryErr  error
	embeddedRegistryOnce sync.Once
)

// LoadRegistry parses and minimally validates registry YAML.
func LoadRegistry(data []byte) (*Registry, error) {
	var reg Registry
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return nil, err
	}
	if reg.Version < 1 {
		return nil, fmt.Errorf("registry version must be >= 1")
	}
	if len(reg.Capabilities) == 0 {
		return nil, fmt.Errorf("registry has no capabilities")
	}
	reg.byID = map[string]*Capability{}
	// Sort BEFORE indexing: pointers into the slice are only stable once
	// the order is final (sorting after taking element addresses corrupts
	// the index).
	sort.Slice(reg.Capabilities, func(a, b int) bool { return reg.Capabilities[a].ID < reg.Capabilities[b].ID })
	for i := range reg.Capabilities {
		c := &reg.Capabilities[i]
		if c.ID == "" || c.Provider == "" || c.Service == "" {
			return nil, fmt.Errorf("registry entry missing id/provider/service")
		}
		if len(c.Evidence) == 0 {
			return nil, fmt.Errorf("registry entry %q has no evidence", c.ID)
		}
		reg.byID[c.ID] = c
	}
	return &reg, nil
}

// EmbeddedRegistry returns the parsed embedded registry copy.
func EmbeddedRegistry() (*Registry, error) {
	embeddedRegistryOnce.Do(func() {
		embeddedRegistry, embeddedRegistryErr = LoadRegistry(registryBytes)
	})
	return embeddedRegistry, embeddedRegistryErr
}

func (r *Registry) find(id string) *Capability {
	if r == nil {
		return nil
	}
	return r.byID[id]
}

// evidenceStrength: "none" (no evidence -> unknown), "docs" (documentation
// only -> conditional, assumption remains), "full" (integration test present).
func evidenceStrength(c *Capability) string {
	if c == nil || len(c.Evidence) == 0 {
		return "none"
	}
	for _, e := range c.Evidence {
		if e.Type == "integration_test" {
			return "full"
		}
	}
	return "docs"
}

func targetRef(c *Capability) string {
	return fmt.Sprintf("%s (%s)", c.Service, c.Provider)
}

// gate applies the evidence rule for a located entry. Verdicts never depend
// on service-name similarity: names appear only in target_capability prose.
func gate(c *Capability, target, requirement string) Check {
	base := Check{Requirement: requirement, Evidence: []EvidenceRef{}, Assumptions: []string{}}
	if c == nil {
		base.Status = "unknown"
		base.TargetCapability = "unresolved (" + target + ")"
		base.Assumptions = []string{"capability absent from registry; cannot establish compatibility"}
		return base
	}
	base.TargetCapability = targetRef(c)
	base.Evidence = append([]EvidenceRef{}, c.Evidence...)
	if c.Provider != target {
		base.Status = "unknown"
		base.Assumptions = []string{fmt.Sprintf("registry entry targets %q, requested %q", c.Provider, target)}
		return base
	}
	if !c.Supported {
		base.Status = "block"
		base.Assumptions = []string{"registry marks this capability unsupported on target"}
		return base
	}
	switch evidenceStrength(c) {
	case "none":
		base.Status = "unknown"
		base.Assumptions = []string{"no evidence recorded; unknown, never pass"}
	case "docs":
		base.Status = "conditional"
		base.Assumptions = []string{"documented but not integration-tested; requires validation before cutover"}
	default:
		base.Status = "pass"
	}
	return base
}

// num coerces JSON numbers (float64 from encoding/json, int from tests).
func num(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func strMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// ruleOrder is the fixed evaluation order (deterministic output ordering).
var ruleOrder = []string{
	"compute", "database-engine", "database-network", "network",
	"object", "queue", "identity", "routing", "observability",
}

// Evaluate runs all capability rules over a schema-valid canonical spec.
// Pure function of (spec, registry, target): no clock, no network, no map
// iteration in output paths.
func Evaluate(spec map[string]any, reg *Registry, target string) CompatReport {
	checks := []Check{}
	reqs, _ := strMap(spec["requirements"])

	// compute / kubernetes.multi_zone
	compute, _ := strMap(spec["compute"])
	orch, _ := compute["orchestrator"].(string)
	replicas, okRep := num(compute["replicas"])
	if orch != "kubernetes" || !okRep || replicas < 1 {
		checks = append(checks, Check{
			Requirement:      "compute.orchestrator must be kubernetes with replicas>=1",
			TargetCapability: "unresolved (" + target + ")",
			Status:           "block",
			Evidence:         []EvidenceRef{},
			Assumptions:      []string{"workload compute requirement unsatisfiable on any target"},
		})
	} else {
		checks = append(checks, gate(reg.find("kubernetes.multi_zone"), target,
			"compute: kubernetes replicas>=1 must schedule on target"))
	}

	// database engine + extensions / postgresql.extensions
	db, _ := strMap(spec["database"])
	engine, _ := db["engine"].(string)
	major, _ := db["major_version"].(string)
	if engine != "postgresql" {
		checks = append(checks, Check{
			Requirement:      "database.engine must be postgresql",
			TargetCapability: "unresolved (" + target + ")",
			Status:           "block",
			Evidence:         []EvidenceRef{},
			Assumptions:      []string{fmt.Sprintf("engine %q has no target mapping in v1", engine)},
		})
	} else {
		c := reg.find("postgresql.extensions")
		chk := gate(c, target, "database: postgresql engine/version/extensions must be satisfiable")
		if chk.Status == "pass" && c != nil {
			if want, _ := c.Constraints["major_version"].(string); want != "" && major != "" && major != want {
				chk.Status = "block"
				chk.Assumptions = []string{fmt.Sprintf("workload major_version %q vs target %q", major, want)}
			}
			if exts, ok := db["extensions"].([]any); ok && len(exts) > 0 {
				known, _ := c.Constraints["required_extensions"].([]any)
				knownSet := map[string]bool{}
				for _, e := range known {
					if s, ok := e.(string); ok {
						knownSet[s] = true
					}
				}
				for _, e := range exts {
					if s, ok := e.(string); ok && !knownSet[s] {
						chk.Status = "unknown"
						chk.Assumptions = []string{fmt.Sprintf("extension %q unattested by registry", s)}
						break
					}
				}
			}
		}
		checks = append(checks, chk)
	}

	// database private networking / postgresql.private_networking
	if priv, _ := reqs["private_data_plane"].(bool); !priv {
		checks = append(checks, Check{
			Requirement:      "database: private data plane (not required)",
			TargetCapability: "n/a",
			Status:           "pass",
			Evidence:         []EvidenceRef{},
			Assumptions:      []string{"workload does not require a private data plane"},
		})
	} else {
		checks = append(checks, gate(reg.find("postgresql.private_networking"), target,
			"database: private data-plane connectivity must hold on target"))
	}

	// network / network.private_connectivity
	if priv, _ := reqs["private_data_plane"].(bool); !priv {
		checks = append(checks, Check{
			Requirement:      "network: private connectivity (not required)",
			TargetCapability: "n/a",
			Status:           "pass",
			Evidence:         []EvidenceRef{},
			Assumptions:      []string{"workload does not require a private data plane"},
		})
	} else {
		checks = append(checks, gate(reg.find("network.private_connectivity"), target,
			"network: private connectivity, routes, isolation must hold on target"))
	}

	// object storage / object.storage
	obj, _ := strMap(spec["object_storage"])
	if req, _ := obj["required"].(bool); !req {
		checks = append(checks, Check{
			Requirement:      "object storage (not required)",
			TargetCapability: "n/a",
			Status:           "pass",
			Evidence:         []EvidenceRef{},
			Assumptions:      []string{"workload does not require object storage"},
		})
	} else {
		chk := gate(reg.find("object.storage"), target,
			"object storage: logical-key+hash manifest reconciliation must be implementable")
		if chk.Status == "pass" {
			chk.Assumptions = append(chk.Assumptions,
				"provider version IDs are not part of cross-cloud identity; content validated by hash")
		}
		checks = append(checks, chk)
	}

	// queue / queue.at_least_once
	q, _ := strMap(spec["queue"])
	delivery, _ := q["delivery_semantics"].(string)
	dupSafe, _ := q["duplicate_safe_consumer"].(bool)
	if delivery != "at_least_once" || !dupSafe {
		checks = append(checks, Check{
			Requirement:      "queue: at-least-once with duplicate-safe consumer",
			TargetCapability: "unresolved (" + target + ")",
			Status:           "block",
			Evidence:         []EvidenceRef{},
			Assumptions:      []string{"v1 supports only at-least-once with duplicate-safe consumers"},
		})
	} else {
		checks = append(checks, gate(reg.find("queue.at_least_once"), target,
			"queue: at-least-once delivery with duplicate-safe consumer must hold"))
	}

	// identity / routing / observability: registry gates over workload-independent needs
	checks = append(checks, gate(reg.find("identity.workload"), target,
		"identity: least-privilege workload identity must be reproducible"))
	checks = append(checks, gate(reg.find("routing.weighted_canary"), target,
		"routing: weighted read-only canary must be implementable"))
	checks = append(checks, gate(reg.find("observability.metrics"), target,
		"observability: metrics/logs/traces with run correlation must be obtainable"))

	if len(checks) != len(ruleOrder) {
		panic("evaluator rule count diverged from rule order")
	}
	return CompatReport{
		TargetProvider:   target,
		Status:           Aggregate(checks),
		Checks:           checks,
		RegistryVersion:  reg.Version,
		EvaluatorVersion: EvaluatorVersion,
	}
}

// Aggregate applies block > unknown > conditional > pass. Never converts
// unknown to conditional or block to pass.
func Aggregate(checks []Check) string {
	overall := "pass"
	for _, c := range checks {
		switch c.Status {
		case "block":
			return "block"
		case "unknown":
			if overall != "block" {
				overall = "unknown"
			}
		case "conditional":
			if overall == "pass" {
				overall = "conditional"
			}
		}
	}
	return overall
}

// ReportID deterministically identifies an evaluation for idempotent
// persistence: same (workload, registry version, evaluator, spec) -> same ID,
// formatted as UUID. created_at is stored alongside but never part of identity.
func ReportID(workloadID string, regVersion int, specHash string) string {
	hexStr := sha256HexCompat([]byte(workloadID + "|" + itoa(regVersion) + "|" + EvaluatorVersion + "|" + specHash))
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexStr[0:8], hexStr[8:12], hexStr[12:16], hexStr[16:20], hexStr[20:32])
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }

// reportBytes marshals a report for HTTP responses and idempotency storage.
func reportBytes(rep CompatReport) ([]byte, error) {
	return json.Marshal(rep)
}

// postCompat implements POST /v1/workloads/{id}/compatibility.
// Evaluation is deterministic analysis: no provisioning, no mutation, no
// policy bypass. Reports persist as derived records (DOMAIN_MODEL entity);
// evaluations themselves are not audited (analysis, not a mutation).
func postCompat(w http.ResponseWriter, r *http.Request, workloadID string) {
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
		TargetRegion   string `json:"target_region"`
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
	rep := Evaluate(spec, reg, in.TargetProvider)
	rep.ID = ReportID(workloadID, reg.Version, specHash)
	rep.WorkloadID = workloadID
	if err := store.SaveCompatReport(rep); err != nil {
		writeErr(w, rid, "INTERNAL_ERROR", "report persist failed", 500)
		return
	}
	resp, _ := reportBytes(rep)
	store.SaveIdem(key, hash, resp, 202)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(202)
	_, _ = w.Write(resp)
}

// getCompat implements GET /v1/workloads/{id}/compatibility (latest report).
func getCompat(w http.ResponseWriter, r *http.Request, workloadID string) {
	rid := reqID(r)
	if _, ok := store.GetWorkload(workloadID); !ok {
		writeErr(w, rid, "NOT_FOUND", "workload not found", 404)
		return
	}
	rep, ok := store.GetLatestCompatReport(workloadID)
	if !ok {
		writeErr(w, rid, "NOT_FOUND", "no compatibility report yet for workload", 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rep)
}
