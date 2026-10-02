// Local/dev authentication + object-level authorization (H-1, H-2).
//
// Local-first deterministic shared-secret authentication for the v1 lab:
// callers present `Authorization: Bearer <token>`; the server maps the token
// to a principal (actor_id, actor_type, admin) configured via the
// SKYBRIDGE_LOCAL_TOKENS environment variable (a JSON array):
//
//	SKYBRIDGE_LOCAL_TOKENS='[{"token":"<random>","actor_id":"op-1","actor_type":"human","admin":true}]'
//
// This is LOCAL/DEV authentication, NOT production identity infrastructure:
// no OIDC, no mTLS, no rotation, no centralized user store. It replaces
// header trust — X-Actor-Id / X-Actor-Type headers are NEVER consulted for
// identity (they are ignored entirely) — with server-side verified material,
// and binds every audit record to the verified principal (M-4).
// Production use requires OIDC/mTLS (DEFERRED; see docs/SECURITY.md).
//
// Fail-closed: with no configured tokens every mutation answers 401.
// No default tokens exist in non-test code; no secret is hardcoded here.
// Tests install fixtures via setTestPrincipals; scripts mint per-run
// random tokens and pass them to the server process and to curl.
//
// Mutation pipeline (explicit, in every mutating handler):
//
//	Authentication (401) -> Authorization (403) -> Policy ->
//	Approval -> Fresh evidence / staleness checks -> Idempotency -> Execution
//
// Neither AI output nor request headers can bypass this path.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
)

// Principal is the verified identity of an authenticated caller. ID and Type
// come from server-side token configuration, never from request headers.
type Principal struct {
	ID    string // verified actor_id
	Type  string // verified actor_type: "human" (default) or "agent"
	Admin bool   // may act on any workload; non-admins only on owned workloads
}

// tokenEntry is one configured bearer token. The token is the authentication
// material; actor_id/actor_type/admin describe the bound principal.
type tokenEntry struct {
	Token     string `json:"token"`
	ActorID   string `json:"actor_id"`
	ActorType string `json:"actor_type"`
	Admin     bool   `json:"admin"`
}

var authMu sync.RWMutex
var authTokens map[string]Principal // token -> principal (process-local)
var authLoaded bool

// loadAuthTokens parses SKYBRIDGE_LOCAL_TOKENS once (fail-closed on empty or
// malformed: the registry stays empty and every mutation is 401).
func loadAuthTokens() {
	authMu.Lock()
	defer authMu.Unlock()
	if authLoaded {
		return
	}
	authLoaded = true
	authTokens = map[string]Principal{}
	raw := strings.TrimSpace(os.Getenv("SKYBRIDGE_LOCAL_TOKENS"))
	if raw == "" {
		return
	}
	var entries []tokenEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return
	}
	for _, e := range entries {
		if e.Token == "" || e.ActorID == "" {
			continue
		}
		typ := e.ActorType
		if typ == "" {
			typ = "human"
		}
		authTokens[e.Token] = Principal{ID: e.ActorID, Type: typ, Admin: e.Admin}
	}
}

// setTestPrincipals replaces the token registry (tests only; same-process
// callers use this to install deterministic fixtures). Pass nil to clear.
func setTestPrincipals(entries []tokenEntry) {
	authMu.Lock()
	defer authMu.Unlock()
	authLoaded = true
	authTokens = map[string]Principal{}
	for _, e := range entries {
		if e.Token == "" || e.ActorID == "" {
			continue
		}
		typ := e.ActorType
		if typ == "" {
			typ = "human"
		}
		authTokens[e.Token] = Principal{ID: e.ActorID, Type: typ, Admin: e.Admin}
	}
}

// bearerToken extracts the bearer credential. Anything else (missing header,
// non-bearer scheme, empty token) is unauthenticated — never a default.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return "", false
	}
	tok := strings.TrimSpace(parts[1])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// authenticate verifies the bearer token against server-side configuration.
// Comparison is constant-time over the presented token length class; unknown
// tokens fail closed. Header-claimed identity is never consulted.
func authenticate(r *http.Request) (Principal, bool) {
	loadAuthTokens()
	tok, ok := bearerToken(r)
	if !ok {
		return Principal{}, false
	}
	authMu.RLock()
	defer authMu.RUnlock()
	for stored, p := range authTokens {
		if len(stored) != len(tok) {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(stored), []byte(tok)) == 1 {
			return p, true
		}
	}
	return Principal{}, false
}

// requireAuth enforces authentication: missing/invalid credentials -> 401
// UNAUTHENTICATED. No mutation proceeds without a verified principal.
func requireAuth(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	p, ok := authenticate(r)
	if !ok {
		writeErr(w, reqID(r), "UNAUTHENTICATED", "valid bearer credentials required", 401)
		return Principal{}, false
	}
	return p, true
}

// workloadOwner returns the principal that registered the workload (""
// when the record predates ownership or the workload is unknown).
func workloadOwner(workloadID string) string {
	wl, ok := store.GetWorkload(workloadID)
	if !ok {
		return ""
	}
	owner, _ := wl["owner_id"].(string)
	return owner
}

// canActOnWorkload reports whether the principal is authorized for the
// workload: admins may act on any workload; anyone else only on workloads
// they registered (owner_id). Records without an owner are admin-only
// (fail-closed for pre-auth rows).
func canActOnWorkload(p Principal, workloadID string) bool {
	if p.Admin {
		return true
	}
	owner := workloadOwner(workloadID)
	return owner != "" && owner == p.ID
}

// requireWorkloadAccess enforces object-level authorization for a workload:
// unknown workload -> 404; authenticated but unauthorized -> 403.
func requireWorkloadAccess(w http.ResponseWriter, r *http.Request, p Principal, workloadID string) bool {
	if _, ok := store.GetWorkload(workloadID); !ok {
		writeErr(w, reqID(r), "NOT_FOUND", "workload not found", 404)
		return false
	}
	if !canActOnWorkload(p, workloadID) {
		writeErr(w, reqID(r), "FORBIDDEN", "authenticated actor is not authorized for this workload", 403)
		return false
	}
	return true
}

// requireMigrationAccess enforces the caller -> principal -> workload ->
// migration -> mutation chain: the migration must exist and bind to exactly
// one workload (migration -> workload binding; mismatches are rejected), and
// the principal must be authorized for that workload. When the route carries
// an explicit workloadID it must match the binding (else 404, as before).
// Returns the bound workload ID.
func requireMigrationAccess(w http.ResponseWriter, r *http.Request, p Principal, migrationID, routeWorkloadID string) (string, bool) {
	rid := reqID(r)
	mig, ok := store.GetMigration(migrationID)
	if !ok {
		writeErr(w, rid, "NOT_FOUND", "migration not found", 404)
		return "", false
	}
	wid, _ := mig["workload_id"].(string)
	if wid == "" {
		writeErr(w, rid, "NOT_FOUND", "migration not found", 404)
		return "", false
	}
	if routeWorkloadID != "" && wid != routeWorkloadID {
		writeErr(w, rid, "NOT_FOUND", "migration does not belong to workload", 404)
		return "", false
	}
	if _, ok := store.GetWorkload(wid); !ok {
		writeErr(w, rid, "NOT_FOUND", "workload not found", 404)
		return "", false
	}
	if !canActOnWorkload(p, wid) {
		writeErr(w, rid, "FORBIDDEN", "authenticated actor is not authorized for this migration", 403)
		return "", false
	}
	return wid, true
}
