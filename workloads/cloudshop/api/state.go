// CloudShop durable admin state (H-3 crash-window fix).
//
// The authoritative-writer flag and quiesce flag previously lived only in
// process memory: a restart silently reset them to environment defaults,
// which could resurrect a second writer (store says azure, restarted source
// says aws) — memory and persisted facts disagreeing.
//
// Admin state is now durable with persist-then-swap semantics: every
// mutation persists first and swaps memory only on success; startup loads
// the persisted state (persisted wins over env defaults). A process failure
// therefore always recovers to the last committed state — paused, never
// dual-authoritative. Persist failure fails the mutation (503) with memory
// unchanged, so memory and persisted state can never diverge.
//
// Backends (first match wins):
//  1. File (CLOUDSHOP_STATE_FILE): atomic JSON (temp + rename, 0600).
//     Used by tests and PG-less deployments that need crash safety.
//  2. Postgres (DATABASE_URL): single-row admin_state table (ensured at
//     connect in NewPGStore). Used by the demo/compose deployment.
//  3. Memory (neither configured): env defaults only. LAB-ONLY — a restart
//     loses transfers; documented, never for real authority.
//
// Corrupt persisted state fails startup (fail-closed, never defaulted).
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// adminState is the durable pair: who owns writes + whether writes pause.
type adminState struct {
	WriteOwnership string `json:"write_ownership"`
	Quiesced       bool   `json:"quiesced"`
}

// stateFile, when set, selects the file backend.
func stateFile() string {
	return strings.TrimSpace(os.Getenv("CLOUDSHOP_STATE_FILE"))
}

// durableBackend reports which backend guards admin state.
func durableBackend() string {
	if stateFile() != "" {
		return "file"
	}
	if _, ok := store.(*PGStore); ok {
		return "postgres"
	}
	return "memory"
}

// validAdminState rejects anything but aws|azure ownership.
func validAdminState(st adminState) bool {
	return st.WriteOwnership == "aws" || st.WriteOwnership == "azure"
}

// persistAdminState durably records st. Callers must hold quiesceMu (the
// memory swap happens only after this returns nil). No durable backend ->
// no-op (memory mode).
func persistAdminState(st adminState) error {
	if !validAdminState(st) {
		return fmt.Errorf("refusing to persist invalid ownership %q", st.WriteOwnership)
	}
	if f := stateFile(); f != "" {
		return persistStateFile(f, st)
	}
	if pg, ok := store.(*PGStore); ok {
		return pg.SaveAdminState(st.WriteOwnership, st.Quiesced)
	}
	return nil
}

// loadAdminState reads the durable state. Returns ok=false when no durable
// backend is configured or it holds no state yet. Corrupt/unreadable state
// on a configured backend is an error (fail-closed at startup).
func loadAdminState() (st adminState, ok bool, err error) {
	if f := stateFile(); f != "" {
		return loadStateFile(f)
	}
	if pg, ok := store.(*PGStore); ok {
		return pg.LoadAdminState()
	}
	return adminState{}, false, nil
}

// initAdminState restores durable admin state at startup (persisted wins
// over env defaults). A configured-but-unreadable backend is fatal: starting
// with defaults could resurrect a second writer.
func initAdminState() {
	st, ok, err := loadAdminState()
	if err != nil {
		log.Fatalf("admin state backend %s unreadable: %v", durableBackend(), err)
	}
	quiesceMu.Lock()
	defer quiesceMu.Unlock()
	if ok {
		if !validAdminState(st) {
			log.Fatalf("admin state backend %s corrupt (ownership %q)", durableBackend(), st.WriteOwnership)
		}
		writeOwnership, quiesced = st.WriteOwnership, st.Quiesced
		log.Printf("admin state restored (backend=%s ownership=%s quiesced=%v)",
			durableBackend(), writeOwnership, quiesced)
		return
	}
	// No prior state: anchor env defaults into the durable backend so the
	// first transfer has a recovery baseline. Anchoring failure is fatal
	// (durability was requested but cannot be provided).
	if durableBackend() != "memory" {
		if err := persistAdminState(adminState{WriteOwnership: writeOwnership, Quiesced: quiesced}); err != nil {
			log.Fatalf("admin state backend %s unwritable: %v", durableBackend(), err)
		}
	}
	log.Printf("admin state initialized (backend=%s ownership=%s quiesced=%v)",
		durableBackend(), writeOwnership, quiesced)
}

// persistStateFile atomically writes st (temp + rename, 0600).
func persistStateFile(path string, st adminState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".admin-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// loadStateFile reads st; missing file means no state yet (ok=false).
func loadStateFile(path string) (adminState, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return adminState{}, false, nil
		}
		return adminState{}, false, err
	}
	var st adminState
	if err := json.Unmarshal(raw, &st); err != nil {
		return adminState{}, false, fmt.Errorf("corrupt admin state file: %w", err)
	}
	if !validAdminState(st) {
		return adminState{}, false, fmt.Errorf("corrupt admin state file: ownership %q", st.WriteOwnership)
	}
	return st, true, nil
}
