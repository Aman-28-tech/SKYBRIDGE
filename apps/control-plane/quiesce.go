// Write-quiesce orchestration: pause new authoritative writes on the source
// CloudShop so CDC can drain to a measured catch-up point. AWS stays
// authoritative throughout this slice; quiesce never transfers ownership.
//
// Model: the control plane drives the CloudShop admin endpoint through the
// QuiesceClient seam (live HTTP by default, fakes in tests). Transitions are
// idempotent (same key + same body replays; same key + different body is
// 409) and serialized per migration. Every transition is audited. The
// CloudShop side (flag + 503 WRITES_PAUSED + readyz.quiesced) lives in
// workloads/cloudshop/api.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// QuiesceClient is the seam to a CloudShop admin surface (source or target).
type QuiesceClient interface {
	SetQuiesced(ctx context.Context, quiesced bool) (bool, error)
	State(ctx context.Context) (quiesced bool, writeOwnership string, err error)
	// SetOwnership flips the authoritative writer (local-only admin endpoint).
	SetOwnership(ctx context.Context, owner string) (string, error)
}

// quiesceClient drives the source CloudShop; targetQuiesceClient drives the
// target. Live HTTP by default; tests inject fakes.
var quiesceClient QuiesceClient = newLiveQuiesceClient("")
var targetQuiesceClient QuiesceClient = newLiveQuiesceClient("-target")

type liveQuiesceClient struct {
	baseURL string
	client  *http.Client
	// adminToken is the CloudShop admin bearer credential (H-3): the control
	// plane authenticates to the CloudShop admin surface; without it every
	// admin call fails closed with 401. Configured via CLOUDSHOP_ADMIN_TOKEN
	// (operator-provided, never hardcoded).
	adminToken string
}

// adminTokenFromEnv reads the operator-provided CloudShop admin credential.
func adminTokenFromEnv() string {
	return os.Getenv("CLOUDSHOP_ADMIN_TOKEN")
}

// suffix "" reads CLOUDSHOP_BASE_URL (source); "-target" reads
// CLOUDSHOP_TARGET_BASE_URL (target, default :8086).
func newLiveQuiesceClient(suffix string) QuiesceClient {
	env := "CLOUDSHOP_BASE_URL"
	def := "http://localhost:8081"
	if suffix != "" {
		env = "CLOUDSHOP_TARGET_BASE_URL"
		def = "http://localhost:8086"
	}
	base := getenv(env, def)
	return &liveQuiesceClient{baseURL: base, client: &http.Client{Timeout: 15 * time.Second}, adminToken: adminTokenFromEnv()}
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// setAdminAuth attaches the CloudShop admin bearer credential (H-3).
func (l *liveQuiesceClient) setAdminAuth(req *http.Request) {
	if l.adminToken == "" {
		if t := adminTokenFromEnv(); t != "" {
			l.adminToken = t
		}
	}
	if l.adminToken != "" {
		req.Header.Set("Authorization", "Bearer "+l.adminToken)
	}
}

func (l *liveQuiesceClient) SetQuiesced(ctx context.Context, quiesced bool) (bool, error) {
	body, _ := json.Marshal(map[string]any{"quiesced": quiesced})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		l.baseURL+"/v1/admin/quiesce", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	l.setAdminAuth(req)
	resp, err := l.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("quiesce request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("quiesce request: status %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		Quiesced bool `json:"quiesced"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, err
	}
	return out.Quiesced, nil
}

func (l *liveQuiesceClient) SetOwnership(ctx context.Context, owner string) (string, error) {
	body, _ := json.Marshal(map[string]any{"write_ownership": owner})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		l.baseURL+"/v1/admin/ownership", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	l.setAdminAuth(req)
	resp, err := l.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ownership request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("ownership request: status %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		WriteOwnership string `json:"write_ownership"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.WriteOwnership, nil
}

func (l *liveQuiesceClient) State(ctx context.Context) (bool, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.baseURL+"/v1/admin/quiesce", nil)
	if err != nil {
		return false, "", err
	}
	l.setAdminAuth(req)
	resp, err := l.client.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("quiesce state: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return false, "", fmt.Errorf("quiesce state: status %d", resp.StatusCode)
	}
	var out struct {
		Quiesced       bool   `json:"quiesced"`
		WriteOwnership string `json:"write_ownership"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, "", err
	}
	return out.Quiesced, out.WriteOwnership, nil
}

// postQuiesce implements POST /v1/migrations/{id}/quiesce.
// Body: {"quiesced":bool}. Repeated identical transitions replay; the
// CloudShop side is itself idempotent (same state -> same 200).
func postQuiesce(w http.ResponseWriter, r *http.Request, workloadID, migrationID string) {
	rid := reqID(r)
	actor, ok := requireAuth(w, r)
	if !ok {
		return
	}
	if _, ok := requireMigrationAccess(w, r, actor, migrationID, workloadID); !ok {
		return
	}
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
		Quiesced *bool `json:"quiesced"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || in.Quiesced == nil {
		writeErr(w, rid, "VALIDATION_FAILED", "quiesced (bool) required", 400)
		return
	}
	mu := idemLock("quiesce:" + migrationID)
	mu.Lock()
	defer mu.Unlock()
	if resp, status, storedHash, found := store.CheckIdem("quiesce:"+key); found {
		if storedHash == "" || storedHash == hash {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(resp)
			return
		}
		idempotencyConflict(w, actor, "set_quiesce", key, rid, AuditEntry{WorkloadID: workloadID, RunID: migrationID})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	state, err := quiesceClient.SetQuiesced(ctx, *in.Quiesced)
	if err != nil {
		writeErr(w, rid, "QUIESCE_UNAVAILABLE", "quiesce transition failed: "+err.Error(), 503)
		return
	}
	_ = store.RecordAudit(AuditEntry{
		RunID: migrationID, WorkloadID: workloadID, ActorType: actor.Type, ActorID: actor.ID,
		Action: "set_quiesce", RequestID: rid, IdempotencyKey: key,
		PolicyBundleVersion: policyBundleVersion(), Result: "success",
		Metadata: fmt.Sprintf(`{"quiesced":%v}`, state),
	})
	body, _ := json.Marshal(map[string]any{
		"migration_id": migrationID, "quiesced": state, "request_id": rid,
	})
	store.SaveIdem("quiesce:"+key, hash, body, 200)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(body)
}
