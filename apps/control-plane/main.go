// Control-plane v1 baseline — stdlib only.
// Implements OpenAPI subset: workloads register/get/list, compatibility report
// (rule-based stub over canonical-workload fixture), drift list/trigger stub,
// migrations create/get, cutover stage request with policy gate mirroring
// packages/contracts/policy/cutover.rego, rollback with post-write block.
// Durability: in-memory Phase 2 baseline; Postgres + Temporal land in Phase 8.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
)

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func writeErr(w http.ResponseWriter, reqID, code, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": code, "message": msg, "request_id": reqID, "details": map[string]any{}}})
}

func reqID(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); v != "" {
		return v
	}
	return "req_" + uuid()[:8]
}

// ---------- store (MemStore default; PGStore when DATABASE_URL set) ----------
var store Store = NewMemStore()

// idemLocks serializes the check -> create -> save critical section per
// Idempotency-Key within this process, so concurrent duplicate submissions
// cannot create two resources. (Single control-plane instance in v1; the PG
// unique key on idempotency_records is the cross-instance backstop.)
var idemLocks sync.Map // key -> *sync.Mutex

func idemLock(key string) *sync.Mutex {
	m, _ := idemLocks.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// canonicalHash returns hex(SHA-256(canonical JSON)) of raw, where canonical
// JSON is the re-marshal of the decoded value (object keys sorted). This is
// the fingerprint required by docs/CLOUDSHOP_API.md.
func canonicalHash(raw []byte) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	canon, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// idempotencyConflict writes the contract-mandated 409 and records the denied
// mutation in the audit trail (FR-014; DOMAIN_MODEL result includes denied;
// no policy is evaluated, so policy_decision stays unset). Actor identity is
// the verified principal (M-4): never header-claimed.
func idempotencyConflict(w http.ResponseWriter, p Principal, action, key, rid string, auditBase AuditEntry) {
	auditBase.Action = action
	auditBase.ActorType = p.Type
	auditBase.ActorID = p.ID
	auditBase.RequestID = rid
	auditBase.IdempotencyKey = key
	auditBase.PolicyBundleVersion = policyBundleVersion()
	auditBase.Result = "denied"
	_ = store.RecordAudit(auditBase)
	writeErr(w, rid, "IDEMPOTENCY_CONFLICT", "same Idempotency-Key with different request body", 409)
}

func policyBundleVersion() string {
	if v := os.Getenv("POLICY_BUNDLE_VERSION"); v != "" {
		return v
	}
	return "dev"
}

// ---------- policy gate lives in policy.go ----------
// policyDecide mirrors packages/contracts/policy/cutover.rego; buildPolicyInput
// assembles its inputs from stored evidence. See policy.go.

// ---------- handlers ----------
func registerWorkload(w http.ResponseWriter, r *http.Request) {
	rid := reqID(r)
	// Authorization pipeline: authentication first (fail-closed 401).
	actor, ok := requireAuth(w, r)
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 {
		writeErr(w, rid, "VALIDATION_FAILED", "Idempotency-Key required (min 16)", 400)
		return
	}
	var raw []byte
	var err error
	if raw, err = io.ReadAll(r.Body); err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "unreadable body", 400)
		return
	}
	hash, err := canonicalHash(raw)
	if err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "invalid JSON", 400)
		return
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "invalid JSON", 400)
		return
	}
	if spec["schema_version"] != float64(1) && spec["schema_version"] != 1 {
		writeErr(w, rid, "VALIDATION_FAILED", "schema_version must be 1", 400)
		return
	}
	// Authoritative validation BEFORE idempotency handling or persistence:
	// invalid requests create no workload, save no idempotency record, and
	// therefore cannot poison a later valid retry under the same key.
	// (No audit event: consistent with the existing invalid-JSON path — no
	// mutation was attempted and no policy evaluated. See docs/IDEMPOTENCY.md.)
	if failures := ValidateCanonical(spec); failures != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "VALIDATION_FAILED", "message": "canonical workload does not conform to schema",
			"request_id": rid, "details": map[string]any{"failures": failures}}})
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
		idempotencyConflict(w, actor, "register_workload", key, rid, AuditEntry{})
		return
	}
	id := uuid()
	// Persist the full submitted specification as canonical_spec (DOMAIN_MODEL:
	// canonical_spec MUST validate against canonical-workload.schema.json).
	// owner_id records the verified registering principal: only the owner
	// (or an admin) may later mutate this workload (H-2).
	wl := map[string]any{"id": id, "name": spec["name"], "schema_version": 1,
		"lifecycle_state": "registered", "canonical_spec": spec, "owner_id": actor.ID}
	if err := store.CreateWorkload(id, wl); err != nil {
		writeErr(w, rid, "INTERNAL_ERROR", "workload persist failed", 500)
		return
	}
	resp, _ := json.Marshal(wl)
	store.SaveIdem(key, hash, resp, 201)
	_ = store.RecordAudit(AuditEntry{
		WorkloadID: id, ActorType: actor.Type, ActorID: actor.ID,
		Action: "register_workload", RequestID: rid, IdempotencyKey: key,
		PolicyBundleVersion: policyBundleVersion(), PolicyDecision: "allow", Result: "success",
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	_, _ = w.Write(resp)
}

// actorID/actorType header trust was removed (H-1): X-Actor-Id and
// X-Actor-Type headers are never consulted for identity. Identity comes only
// from authenticate() (Bearer token -> server-side principal), and audit
// records carry the verified principal (M-4).

// migrationScope resolves a migration to its (workloadID, migrationID).
func migrationScope(migrationID string) (string, string, bool) {
	mig, ok := store.GetMigration(migrationID)
	if !ok {
		return "", "", false
	}
	wid, _ := mig["workload_id"].(string)
	if wid == "" {
		return "", "", false
	}
	return wid, migrationID, true
}

// workloadRouter dispatches /v1/workloads/{id} and sub-resources.
func workloadRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/workloads/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 2 && parts[1] == "compatibility" {
		switch r.Method {
		case http.MethodGet:
			getCompat(w, r, parts[0])
		case http.MethodPost:
			postCompat(w, r, parts[0])
		default:
			writeErr(w, reqID(r), "NOT_FOUND", "not found", 404)
		}
		return
	}
	if len(parts) == 2 && parts[1] == "plan" {
		switch r.Method {
		case http.MethodGet:
			getPlanLatest(w, r, parts[0])
		case http.MethodPost:
			postPlan(w, r, parts[0])
		default:
			writeErr(w, reqID(r), "NOT_FOUND", "not found", 404)
		}
		return
	}
	if len(parts) == 2 && parts[1] == "drift" {
		switch r.Method {
		case http.MethodGet:
			getDriftList(w, r, parts[0])
		case http.MethodPost:
			postDrift(w, r, parts[0])
		default:
			writeErr(w, reqID(r), "NOT_FOUND", "not found", 404)
		}
		return
	}
	if len(parts) == 2 && parts[1] == "readiness" {
		if r.Method == http.MethodPost {
			postReadiness(w, r, parts[0])
		} else {
			writeErr(w, reqID(r), "NOT_FOUND", "not found", 404)
		}
		return
	}
	if len(parts) == 2 && parts[1] == "approvals" {
		switch r.Method {
		case http.MethodGet:
			getApprovalList(w, r, parts[0])
		case http.MethodPost:
			postApproval(w, r, parts[0])
		default:
			writeErr(w, reqID(r), "NOT_FOUND", "not found", 404)
		}
		return
	}
	if len(parts) == 4 && parts[1] == "approvals" && parts[3] == "decision" {
		if r.Method == http.MethodPost {
			decideApproval(w, r, parts[0], parts[2])
		} else {
			writeErr(w, reqID(r), "NOT_FOUND", "not found", 404)
		}
		return
	}
	if len(parts) == 2 && parts[1] == "migrations" {
		if r.Method == http.MethodGet {
			listMigrationsForWorkload(w, r, parts[0])
		} else {
			writeErr(w, reqID(r), "NOT_FOUND", "not found", 404)
		}
		return
	}
	getWorkload(w, r)
}

// getMigration implements GET /v1/migrations/{id} (read-only): returns the
// stored migration record. The shape matches the OpenAPI MigrationRun
// schema (id, workload_id, status, current_step, ...).
func getMigration(w http.ResponseWriter, r *http.Request, id string) {
	rid := reqID(r)
	mig, ok := store.GetMigration(id)
	if !ok {
		writeErr(w, rid, "NOT_FOUND", "migration not found", 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(mig)
}

// getMigrationAudit implements GET /v1/migrations/{id}/audit (read-only):
// chronological audit entries scoped to the migration. Unknown migration
// is 404 (never an empty-timeline misread); entries expose stage, status,
// timestamp, request/actor, policy material, approval, and per-stage CDC
// evidence from metadata.
func getMigrationAudit(w http.ResponseWriter, r *http.Request, id string) {
	rid := reqID(r)
	if _, ok := store.GetMigration(id); !ok {
		writeErr(w, rid, "NOT_FOUND", "migration not found", 404)
		return
	}
	items := store.GetAuditForMigration(id)
	if items == nil {
		items = []AuditEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"migration_id": id, "items": items, "request_id": rid,
	})
}

func getWorkload(w http.ResponseWriter, r *http.Request) {
	rid := reqID(r)
	id := strings.TrimPrefix(r.URL.Path, "/v1/workloads/")
	id = strings.Split(id, "/")[0]
	wl, ok := store.GetWorkload(id)
	if !ok {
		writeErr(w, rid, "NOT_FOUND", "workload not found", 404)
		return
	}
	_ = json.NewEncoder(w).Encode(wl)
}

func listWorkloads(w http.ResponseWriter, r *http.Request) {
	items := store.ListWorkloads()
	if items == nil {
		items = []map[string]any{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "next_cursor": nil})
}

func createMigration(w http.ResponseWriter, r *http.Request) {
	rid := reqID(r)
	actor, ok := requireAuth(w, r)
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 {
		writeErr(w, rid, "VALIDATION_FAILED", "Idempotency-Key required", 400)
		return
	}
	var raw []byte
	var err error
	if raw, err = io.ReadAll(r.Body); err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "unreadable body", 400)
		return
	}
	hash, err := canonicalHash(raw)
	if err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "invalid JSON", 400)
		return
	}
	var in struct {
		WorkloadID string `json:"workload_id"`
		Target     string `json:"target_provider"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || in.WorkloadID == "" {
		writeErr(w, rid, "VALIDATION_FAILED", "workload_id required", 400)
		return
	}
	if in.Target != "azure" {
		writeErr(w, rid, "VALIDATION_FAILED", "target_provider must be azure", 400)
		return
	}
	// Object authorization (H-2): the caller must be authorized for the
	// workload the migration will bind to. Unknown workload -> 404,
	// authenticated but unauthorized -> 403.
	if !requireWorkloadAccess(w, r, actor, in.WorkloadID) {
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
		idempotencyConflict(w, actor, "create_migration", key, rid, AuditEntry{WorkloadID: in.WorkloadID})
		return
	}
	id := uuid()
	run := map[string]any{"id": id, "workload_id": in.WorkloadID, "status": "REGISTERED",
		"current_step": "registered", "request_id": rid, "policy_bundle_version": policyBundleVersion()}
	if err := store.CreateMigration(id, run); err != nil {
		writeErr(w, rid, "INTERNAL_ERROR", "migration persist failed", 500)
		return
	}
	_ = store.RecordAudit(AuditEntry{
		RunID: id, WorkloadID: in.WorkloadID, ActorType: actor.Type, ActorID: actor.ID,
		Action: "create_migration", RequestID: rid, IdempotencyKey: key,
		PolicyBundleVersion: policyBundleVersion(), PolicyDecision: "allow", Result: "success",
	})
	resp, _ := json.Marshal(run)
	store.SaveIdem(key, hash, resp, 202)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(202)
	_, _ = w.Write(resp)
}

func cutover(w http.ResponseWriter, r *http.Request) {
	rid := reqID(r)
	actor, ok := requireAuth(w, r)
	if !ok {
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 {
		writeErr(w, rid, "NOT_FOUND", "not found", 404)
		return
	}
	migID := parts[2]
	// Caller -> principal -> workload -> migration -> mutation (H-2): the
	// migration binds to exactly one workload; mismatches are rejected.
	wid, ok := requireMigrationAccess(w, r, actor, migID, "")
	if !ok {
		return
	}
	var in map[string]any
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "invalid JSON", 400)
		return
	}
	if in == nil {
		in = map[string]any{}
	}
	// Runtime defaults for fields the caller omits; compatibility and drift
	// come from stored evidence via buildPolicyInput (never stubbed).
	for k, v := range map[string]any{
		"action": "shift_traffic", "environment": "dev", "validation_status": "passed",
		"cdc_lag_seconds": 8, "rpo_seconds": 30, "target_healthy": true,
		"read_only_canary": true, "write_ownership": "aws",
	} {
		if _, exists := in[k]; !exists {
			in[k] = v
		}
	}
	pin, fail := buildPolicyInput(wid, in, actor)
	if fail != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fail.Status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": fail.Code, "message": fail.Message,
			"request_id": rid, "details": map[string]any{}}})
		return
	}
	decision, reasons := policyDecide(pin)
	reason := ""
	if len(reasons) > 0 {
		reason = reasons[0]
	}
	audit := AuditEntry{
		RunID: migID, WorkloadID: wid, ActorType: actor.Type, ActorID: actor.ID,
		Action: "shift_traffic", RequestID: rid,
		PolicyBundleVersion: policyBundleVersion(), PolicyDecision: decision,
		Metadata: fmt.Sprintf(`{"policy_input_hash":%q}`, pin.inputHash()),
	}
	viaApproval := false
	if decision == PolicyApprovalRequired {
		// Approval-gated: only a fresh, bound, approved approval permits
		// eligibility. Anything else stays refused.
		approvalID, _ := in["approval_id"].(string)
		if appr, code, msg := approvalEligibleForUse(wid, approvalID, pin); appr == nil {
			audit.Result = "denied"
			_ = store.RecordAudit(audit)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": code, "message": msg, "request_id": rid,
				"details": map[string]any{"deny_reason": reason, "decision": decision}}})
			return
		} else {
			audit.ApprovalID = appr.ID
			viaApproval = true
			decision = PolicyAllow
			reason = "approved: " + appr.ID
		}
	}
	if decision != PolicyAllow {
		audit.Result = "denied"
		_ = store.RecordAudit(audit)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "CUTOVER_DENIED", "message": reason, "request_id": rid,
			"details": map[string]any{"deny_reason": reason, "decision": decision}}})
		return
	}
	h := sha256.Sum256([]byte(r.URL.Path))
	_ = hex.EncodeToString(h[:])
	if !viaApproval {
		audit.PolicyDecision = PolicyAllow
	}
	audit.Result = "success"
	_ = store.RecordAudit(audit)
	w.WriteHeader(202)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"migration_id": migID, "target_weight": in["target_weight"],
		"stage_status": "accepted", "request_id": rid,
		"policy_bundle_version": policyBundleVersion(),
		"policy_input_hash":     pin.inputHash(), "decision": PolicyAllow,
		"approval_id": audit.ApprovalID})
}

func rollback(w http.ResponseWriter, r *http.Request) {
	rid := reqID(r)
	actor, ok := requireAuth(w, r)
	if !ok {
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	migID := ""
	if len(parts) >= 3 {
		migID = parts[2]
	}
	// Migration -> workload binding (H-2): unknown migrations are rejected
	// (404) instead of accepted, and cross-workload/cross-migration use by an
	// unauthorized actor is 403.
	wid, ok := requireMigrationAccess(w, r, actor, migID, "")
	if !ok {
		return
	}
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	// Authoritative ownership wins over caller assertions: after Azure owns
	// writes, traffic-only rollback to AWS is rejected because v1 has no
	// reverse CDC. Recovery is forward-fix from authoritative Azure state.
	if rec, ok := store.GetOwnership(migID); ok && rec.CurrentOwner == "azure" {
		cutoverStats.rollbackDenied()
		cutoverLog("rollback_rejected", migID, "", rid, map[string]any{
			"reason": "reverse CDC unsupported", "current_owner": "azure"})
		_ = store.RecordAudit(AuditEntry{
			RunID: migID, WorkloadID: wid, ActorType: actor.Type, ActorID: actor.ID,
			Action: "rollback_traffic", RequestID: rid,
			PolicyBundleVersion: policyBundleVersion(),
			PolicyDecision:      "deny", Result: "denied",
		})
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "POST_WRITE_ROLLBACK_BLOCKED",
			"message":   "traffic-only rollback to AWS unsupported without reverse CDC; Azure remains authoritative, use forward recovery",
			"request_id": rid, "details": map[string]any{"current_owner": "azure"}}})
		return
	}
	if in != nil && in["write_ownership"] == "azure" && in["reverse_sync_ready"] != true {
		cutoverStats.rollbackDenied()
		cutoverLog("rollback_rejected", migID, "", rid, map[string]any{
			"reason": "reverse CDC unsupported", "current_owner": "azure"})
		_ = store.RecordAudit(AuditEntry{
			RunID: migID, WorkloadID: wid, ActorType: actor.Type, ActorID: actor.ID,
			Action: "rollback_traffic", RequestID: rid,
			PolicyBundleVersion: policyBundleVersion(),
			PolicyDecision:      "deny", Result: "denied",
		})
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "POST_WRITE_ROLLBACK_BLOCKED",
			"message":   "traffic-only rollback blocked after Azure owns writes; use forward-fix",
			"request_id": rid, "details": map[string]any{}}})
		return
	}
	w.WriteHeader(202)
	_ = store.RecordAudit(AuditEntry{
		RunID: migID, WorkloadID: wid, ActorType: actor.Type, ActorID: actor.ID,
		Action: "rollback_traffic", RequestID: rid,
		PolicyBundleVersion: policyBundleVersion(),
		PolicyDecision:      "allow", Result: "success",
	})
	_, _ = w.Write([]byte(`{"stage_status":"accepted"}`))
}

func initStore() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Print("DATABASE_URL unset: using in-memory store")
		return
	}
	pg, err := NewPGStore(url)
	if err != nil {
		log.Printf("postgres unavailable (%v): falling back to in-memory store", err)
		return
	}
	store = pg
	log.Print("using postgres store")
}

func main() {
	initStore()
	defer store.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/v1/workloads", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			listWorkloads(w, r)
		case http.MethodPost:
			registerWorkload(w, r)
		default:
			writeErr(w, reqID(r), "NOT_FOUND", "not found", 404)
		}
	})
	mux.HandleFunc("/v1/workloads/", workloadRouter)
	mux.HandleFunc("/v1/migrations", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			createMigration(w, r)
			return
		}
		writeErr(w, reqID(r), "NOT_FOUND", "not found", 404)
	})
	mux.HandleFunc("/v1/migrations/", func(w http.ResponseWriter, r *http.Request) {
		// Read-only views for the console (no mutations on any method here).
		// GET /v1/migrations/{id}: migration record (status, current_step).
		if r.Method == http.MethodGet {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if len(parts) == 3 {
				getMigration(w, r, parts[2])
				return
			}
			// GET /v1/migrations/{id}/audit: chronological migration-scoped
			// audit trail (timeline source; never an authorization input).
			if len(parts) == 4 && parts[3] == "audit" {
				getMigrationAudit(w, r, parts[2])
				return
			}
			// GET /v1/migrations/{id}/summary: read-only console aggregation
			// (lifecycle, cutover timeline, CDC, ownership, safety). No
			// mutation, no audit write, no policy evaluation.
			if len(parts) == 4 && parts[3] == "summary" {
				getMigrationSummary(w, r, parts[2])
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/cutover") && r.Method == http.MethodGet {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if len(parts) == 4 {
				if wid, migID, ok := migrationScope(parts[2]); ok {
					getCutoverStatus(w, r, wid, migID)
					return
				}
				writeErr(w, reqID(r), "NOT_FOUND", "migration not found", 404)
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/cutover/final") && r.Method == http.MethodPost {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if len(parts) == 5 {
				if wid, migID, ok := migrationScope(parts[2]); ok {
					postCutoverFinal(w, r, wid, migID)
					return
				}
				writeErr(w, reqID(r), "NOT_FOUND", "migration not found", 404)
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/cutover") && r.Method == http.MethodPost {
			cutover(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/rollback") && r.Method == http.MethodPost {
			rollback(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/rehearse") && r.Method == http.MethodPost {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if len(parts) == 4 {
				migID := parts[2]
				if mig, ok := store.GetMigration(migID); ok {
					if wid, _ := mig["workload_id"].(string); wid != "" {
						postRehearse(w, r, wid, migID)
						return
					}
				}
				writeErr(w, reqID(r), "NOT_FOUND", "migration not found", 404)
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/canary") && (r.Method == http.MethodPost || r.Method == http.MethodGet) {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if len(parts) == 4 {
				if wid, migID, ok := migrationScope(parts[2]); ok {
					if r.Method == http.MethodPost {
						postCanary(w, r, wid, migID)
					} else {
						getCanary(w, r, wid, migID)
					}
					return
				}
				writeErr(w, reqID(r), "NOT_FOUND", "migration not found", 404)
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/quiesce") && r.Method == http.MethodPost {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if len(parts) == 4 {
				if wid, migID, ok := migrationScope(parts[2]); ok {
					postQuiesce(w, r, wid, migID)
					return
				}
				writeErr(w, reqID(r), "NOT_FOUND", "migration not found", 404)
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/readiness") && r.Method == http.MethodPost {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if len(parts) == 4 {
				if wid, migID, ok := migrationScope(parts[2]); ok {
					postReadinessForCutover(w, r, wid, migID)
					return
				}
				writeErr(w, reqID(r), "NOT_FOUND", "migration not found", 404)
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/execute") && r.Method == http.MethodPost {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if len(parts) == 4 {
				postExecute(w, r, parts[2])
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/execution") && r.Method == http.MethodGet {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if len(parts) == 4 {
				getExecution(w, r, parts[2])
				return
			}
		}
		writeErr(w, reqID(r), "NOT_FOUND", "not found", 404)
	})
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	// Execution plane: start the in-process Temporal worker when a server is
	// configured. The worker executes pre-authorized workflows only; without
	// it, POST /execute fails closed with TEMPORAL_UNAVAILABLE.
	if host := os.Getenv("TEMPORAL_HOST"); host != "" {
		tc, err := NewRealTemporalClient(host, TemporalNamespace, TemporalTaskQueue)
		if err != nil {
			log.Fatalf("temporal dial %s: %v", host, err)
		}
		temporalClient = tc
		stop, err := StartWorker(tc, store)
		if err != nil {
			log.Fatalf("temporal worker: %v", err)
		}
		defer stop()
		log.Printf("temporal worker running (host=%s queue=%s)", host, TemporalTaskQueue)
	} else {
		log.Print("TEMPORAL_HOST unset: execution endpoint will refuse with TEMPORAL_UNAVAILABLE")
	}
	log.Printf("control-plane listening :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, withCORS(mux)))
}
