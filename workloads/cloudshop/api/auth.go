// CloudShop admin authentication (H-3).
//
// The /v1/admin/* surface (quiesce, ownership) previously accepted any
// network caller. It now requires bearer authentication against
// server-side-configured tokens (CLOUDSHOP_ADMIN_TOKENS, a JSON array):
//
//	CLOUDSHOP_ADMIN_TOKENS='[{"token":"<random>","actor_id":"cp-1","admin":true}]'
//
// - Missing/invalid credentials -> 401 UNAUTHENTICATED (reads and writes).
// - Valid non-admin token -> 403 FORBIDDEN on mutations (reads allowed).
// - Valid admin token -> allowed.
//
// This is LOCAL/DEV authentication, NOT production identity infrastructure
// (same caveat as the control plane; see its auth.go). No default tokens
// exist in non-test code; no secret is hardcoded. With no configured tokens
// the admin surface fails closed (401 on everything).
package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
)

// adminPrincipal is the verified identity of an admin-surface caller.
type adminPrincipal struct {
	ID    string
	Admin bool
}

type adminTokenEntry struct {
	Token   string `json:"token"`
	ActorID string `json:"actor_id"`
	Admin   bool   `json:"admin"`
}

var adminAuthMu sync.RWMutex
var adminAuthTokens map[string]adminPrincipal
var adminAuthLoaded bool

func loadAdminAuthTokens() {
	adminAuthMu.Lock()
	defer adminAuthMu.Unlock()
	if adminAuthLoaded {
		return
	}
	adminAuthLoaded = true
	adminAuthTokens = map[string]adminPrincipal{}
	raw := strings.TrimSpace(os.Getenv("CLOUDSHOP_ADMIN_TOKENS"))
	if raw == "" {
		return
	}
	var entries []adminTokenEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return
	}
	for _, e := range entries {
		if e.Token == "" || e.ActorID == "" {
			continue
		}
		adminAuthTokens[e.Token] = adminPrincipal{ID: e.ActorID, Admin: e.Admin}
	}
}

// setTestAdminTokens replaces the admin token registry (tests only).
func setTestAdminTokens(entries []adminTokenEntry) {
	adminAuthMu.Lock()
	defer adminAuthMu.Unlock()
	adminAuthLoaded = true
	adminAuthTokens = map[string]adminPrincipal{}
	for _, e := range entries {
		if e.Token == "" || e.ActorID == "" {
			continue
		}
		adminAuthTokens[e.Token] = adminPrincipal{ID: e.ActorID, Admin: e.Admin}
	}
}

// authenticateAdmin verifies the bearer token against server-side
// configuration. Unknown/missing credentials fail closed.
func authenticateAdmin(r *http.Request) (adminPrincipal, bool) {
	loadAdminAuthTokens()
	h := r.Header.Get("Authorization")
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return adminPrincipal{}, false
	}
	tok := strings.TrimSpace(parts[1])
	if tok == "" {
		return adminPrincipal{}, false
	}
	adminAuthMu.RLock()
	defer adminAuthMu.RUnlock()
	for stored, p := range adminAuthTokens {
		if len(stored) != len(tok) {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(stored), []byte(tok)) == 1 {
			return p, true
		}
	}
	return adminPrincipal{}, false
}

// requireAdminAuth enforces authentication on the admin surface: no valid
// token -> 401. (Reads need authentication only; mutations additionally
// require the admin capability via requireAdmin.)
func requireAdminAuth(w http.ResponseWriter, r *http.Request) (adminPrincipal, bool) {
	p, ok := authenticateAdmin(r)
	if !ok {
		writeErr(w, reqID(r), "UNAUTHENTICATED", "valid bearer credentials required", 401)
		return adminPrincipal{}, false
	}
	return p, true
}

// requireAdmin enforces the admin capability for admin mutations:
// authenticated but non-admin -> 403.
func requireAdmin(w http.ResponseWriter, r *http.Request, p adminPrincipal) bool {
	if !p.Admin {
		writeErr(w, reqID(r), "FORBIDDEN", "authenticated actor is not authorized for admin mutation", 403)
		return false
	}
	return true
}
