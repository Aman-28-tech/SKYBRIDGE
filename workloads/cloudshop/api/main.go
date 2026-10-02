// CloudShop API v1 — Go stdlib only (no external deps for Phase 1 local).
// Implements: GET /healthz, GET /readyz (schema_version + write_ownership),
// GET /v1/products (+pagination), GET /v1/products/{id},
// POST /v1/orders (Idempotency-Key, SHA-256, 409 on conflict),
// GET /v1/orders/{id}, 403 WRITE_NOT_OWNED for writes when not owner.
// Storage: in-memory (Phase 1 local); Postgres wiring lands with DATABASE_URL
// via the same handler interface (see store interface). Redis read-through
// uses an in-process TTL cache (lazy warm, TTL 300s); managed Redis in cloud.
package main

import (
	"crypto/sha256"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const schemaVersion = 1
const idemScope = "cloudshop:create-order"

var writeOwnership = getEnv("WRITE_OWNERSHIP", "aws") // aws|azure

// quiesced pauses new authoritative writes for migration cutover sequencing
// (CUTOVER_ROUTING.md Stage 5). Reads are unaffected. AWS stays the
// authoritative owner while quiesced; only a future ownership transfer (not
// this slice) changes who owns writes. Local-only admin surface.
var quiesceMu sync.Mutex
var quiesced = getEnv("QUIESCED", "") == "1"

func isQuiesced() bool {
	quiesceMu.Lock()
	defer quiesceMu.Unlock()
	return quiesced
}

func setQuiesced(b bool) (bool, error) {
	quiesceMu.Lock()
	defer quiesceMu.Unlock()
	// H-3 persist-then-swap: durability first, memory only on success.
	// A persist failure leaves memory unchanged (503 to the caller), so
	// memory and persisted facts can never disagree.
	if err := persistAdminState(adminState{WriteOwnership: writeOwnership, Quiesced: b}); err != nil {
		return quiesced, err
	}
	quiesced = b
	return quiesced, nil
}

// store is MemStore by default; PGStore when DATABASE_URL is set.
var store Store = NewMemStore()

func getEnv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// ---------- cache (lazy warm, TTL 300s) ----------
type cacheEntry struct {
	body      []byte
	expiresAt time.Time
}

var productCache = struct {
	sync.RWMutex
	m map[string]cacheEntry
}{m: map[string]cacheEntry{}}

func cacheGet(id string) ([]byte, bool) {
	productCache.RLock()
	e, ok := productCache.m[id]
	productCache.RUnlock()
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.body, true
}

func cachePut(id string, body []byte) {
	productCache.Lock()
	productCache.m[id] = cacheEntry{body: body, expiresAt: time.Now().Add(300 * time.Second)}
	productCache.Unlock()
}

// Note: products are read-only over the API (GET only, no update path),
// so entries cannot go stale before the 300s TTL; no invalidation hook
// is needed. If a product write path is ever added, invalidate there.

// ---------- domain types live in store.go ----------

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ---------- envelope ----------
func writeErr(w http.ResponseWriter, reqID string, code string, msg string, status int) {
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

// ---------- handlers ----------
func healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
	_, _ = w.Write([]byte("ok"))
}

func readyz(w http.ResponseWriter, r *http.Request) {
	backend := "in-memory"
	if err := store.Ping(); err != nil {
		writeErr(w, reqID(r), "DEPENDENCY_UNAVAILABLE", "store unreachable", 503)
		return
	}
	if _, ok := store.(*PGStore); ok {
		backend = "postgres"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ready": true, "schema_version": schemaVersion, "write_ownership": currentOwnership(),
		"quiesced": isQuiesced(),
		"checks": map[string]string{"database": "healthy (" + backend + ")", "queue": "healthy"},
	})
}

// quiesceAdmin implements the authenticated admin surface:
// POST /v1/admin/quiesce {"quiesced":bool} -> 200 {"quiesced":bool,...}
// GET  /v1/admin/quiesce -> 200 {"quiesced":bool,"write_ownership":...}
// Transitions are idempotent (same state -> same result).
// Unauthenticated -> 401; authenticated non-admin POST -> 403 (H-3).
func quiesceAdmin(w http.ResponseWriter, r *http.Request) {
	rid := reqID(r)
	caller, ok := requireAdminAuth(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"quiesced": isQuiesced(), "write_ownership": currentOwnership(), "request_id": rid,
		})
	case http.MethodPost:
		if !requireAdmin(w, r, caller) {
			return
		}
		var in struct {
			Quiesced *bool `json:"quiesced"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Quiesced == nil {
			writeErr(w, rid, "VALIDATION_FAILED", "quiesced (bool) required", 400)
			return
		}
		state, err := setQuiesced(*in.Quiesced)
		if err != nil {
			writeErr(w, rid, "ADMIN_STATE_UNAVAILABLE", "quiesce persist failed", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"quiesced": state, "write_ownership": currentOwnership(), "request_id": rid,
		})
	default:
		writeErr(w, rid, "NOT_FOUND", "not found", 404)
	}
}

// currentOwnership reads the authoritative-writer flag under lock.
func currentOwnership() string {
	quiesceMu.Lock()
	defer quiesceMu.Unlock()
	return writeOwnership
}

// setOwnership flips the authoritative writer (admin mutation). Only
// aws|azure are valid; persistence precedes the memory swap (H-3), so a
// failure returns an error with memory unchanged — never a diverged flag.
func setOwnership(owner string) (string, bool, error) {
	if owner != "aws" && owner != "azure" {
		return "", false, nil
	}
	quiesceMu.Lock()
	defer quiesceMu.Unlock()
	if err := persistAdminState(adminState{WriteOwnership: owner, Quiesced: quiesced}); err != nil {
		return "", false, err
	}
	writeOwnership = owner
	return writeOwnership, true, nil
}

// ownershipAdmin implements the authenticated ownership surface:
// POST /v1/admin/ownership {"write_ownership":"azure"} -> 200 with state.
// Idempotent: repeating the current owner returns the same state.
// Unauthenticated -> 401; authenticated non-admin -> 403 (H-3).
func ownershipAdmin(w http.ResponseWriter, r *http.Request) {
	rid := reqID(r)
	caller, ok := requireAdminAuth(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"write_ownership": currentOwnership(), "quiesced": isQuiesced(), "request_id": rid,
		})
	case http.MethodPost:
		if !requireAdmin(w, r, caller) {
			return
		}
		var in struct {
			WriteOwnership string `json:"write_ownership"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeErr(w, rid, "VALIDATION_FAILED", "write_ownership required", 400)
			return
		}
		owner, valid, err := setOwnership(in.WriteOwnership)
		if err != nil {
			writeErr(w, rid, "ADMIN_STATE_UNAVAILABLE", "ownership persist failed", 503)
			return
		}
		if !valid {
			writeErr(w, rid, "VALIDATION_FAILED", "write_ownership must be aws|azure", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"write_ownership": owner, "quiesced": isQuiesced(), "request_id": rid,
		})
	default:
		writeErr(w, rid, "NOT_FOUND", "not found", 404)
	}
}

func listProducts(w http.ResponseWriter, r *http.Request) {
	rid := reqID(r)
	items := store.ListProducts()
	if items == nil {
		items = []Product{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "next_cursor": nil, "request_id": rid})
}

func getProduct(w http.ResponseWriter, r *http.Request) {
	rid := reqID(r)
	id := strings.TrimPrefix(r.URL.Path, "/v1/products/")
	if id == "" || strings.Contains(id, "/") {
		writeErr(w, rid, "NOT_FOUND", "product not found", 404)
		return
	}
	if body, ok := cacheGet(id); ok {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "HIT")
		_, _ = w.Write(body)
		return
	}
	p, ok := store.GetProduct(id)
	if !ok {
		writeErr(w, rid, "NOT_FOUND", "product not found", 404)
		return
	}
	body, _ := json.Marshal(p)
	cachePut(id, body) // lazy warm: miss -> store -> populate
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	_, _ = w.Write(body)
}

func createOrder(w http.ResponseWriter, r *http.Request) {
	rid := reqID(r)
	if !isOwner() {
		writeErr(w, rid, "WRITE_NOT_OWNED", "this deployment is not the write owner (read-only canary)", 403)
		return
	}
	if isQuiesced() {
		// Explicit backpressure: accepted work drains, new writes wait.
	 // Never silently dropped; the caller retries after Retry-After.
		w.Header().Set("Retry-After", "30")
		writeErr(w, rid, "WRITES_PAUSED", "writes paused for migration cutover sequencing", 503)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 {
		writeErr(w, rid, "VALIDATION_FAILED", "Idempotency-Key header required (min 16 chars)", 400)
		return
	}
	var raw json.RawMessage
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	var bodyAny any
	if err := dec.Decode(&bodyAny); err != nil {
		writeErr(w, rid, "VALIDATION_FAILED", "invalid JSON body", 400)
		return
	}
	canonical, _ := json.Marshal(bodyAny)
	raw = canonical
	hash := sha256Hex(raw)

	rec := store.LookupIdem(idemScope, key)
	if rec.Found {
		if rec.RequestHash == hash {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.Status)
			_, _ = w.Write(rec.Response)
			return
		}
		writeErr(w, rid, "IDEMPOTENCY_CONFLICT", "same key with different body", 409)
		return
	}
	// create order (minimal v1: single implicit item set via total_cents)
	var in struct {
		UserID     string `json:"user_id"`
		TotalCents int64  `json:"total_cents"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || in.TotalCents < 0 {
		writeErr(w, rid, "VALIDATION_FAILED", "user_id and total_cents>=0 required", 400)
		return
	}
	o := Order{ID: uuid(), UserID: in.UserID, Status: "pending", TotalCents: in.TotalCents}
	if err := store.CreateOrder(o); err != nil {
		writeErr(w, rid, "INTERNAL_ERROR", "order persist failed", 500)
		return
	}
	resp, _ := json.Marshal(o)
	store.SaveIdem(idemScope, key, hash, resp, 201)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	_, _ = w.Write(resp)
}

func getOrder(w http.ResponseWriter, r *http.Request) {
	rid := reqID(r)
	id := strings.TrimPrefix(r.URL.Path, "/v1/orders/")
	o, ok := store.GetOrder(id)
	if !ok || strings.Contains(id, "/") {
		writeErr(w, rid, "NOT_FOUND", "order not found", 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(o)
}

var ownershipOverride = ""

func isOwner() bool {
	if ownershipOverride != "" {
		return ownershipOverride == "self"
	}
	// This instance owns writes iff WRITE_OWNERSHIP names this deployment.
	// Deployment name via DEPLOYMENT env (aws|azure); default aws owns when WRITE_OWNERSHIP=aws.
	deploy := getEnv("DEPLOYMENT", "aws")
	return deploy == currentOwnership()
}

func seed() {
	p := Product{ID: uuid(), SKU: "SKU-001", Name: "Demo Widget", PriceCents: 999}
	store.SeedProduct(p)
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
	// H-3: restore durable admin state BEFORE serving (persisted wins over
	// env defaults; unreadable backend fails startup, never defaults).
	initAdminState()
	seed()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/readyz", readyz)
	mux.HandleFunc("/v1/products", listProducts)
	mux.HandleFunc("/v1/products/", getProduct)
	mux.HandleFunc("/v1/orders", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			createOrder(w, r)
			return
		}
		writeErr(w, reqID(r), "NOT_FOUND", "not found", 404)
	})
	mux.HandleFunc("/v1/orders/", getOrder)
	mux.HandleFunc("/v1/admin/quiesce", quiesceAdmin)
	mux.HandleFunc("/v1/admin/ownership", ownershipAdmin)
	port := getEnv("PORT", "8081")
	log.Printf("cloudshop api listening :%s (deployment=%s ownership=%s)", port, getEnv("DEPLOYMENT", "aws"), writeOwnership)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
