// Live target test: proves PostgresStore applies insert/update/delete with
// duplicate safety against a real PostgreSQL (local target :5434).
// Runs only when TARGET_DATABASE_URL is set; hermetic unit tests cover CI.
package cdc

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func liveDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("TARGET_DATABASE_URL")
	if url == "" {
		t.Skip("TARGET_DATABASE_URL unset")
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	ps := NewPostgresStore(db)
	if err := ps.Ensure(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestLiveTargetApply(t *testing.T) {
	db := liveDB(t)
	ps := NewPostgresStore(db)
	// Unique per run: reruns must apply fresh, not hit prior dedupe markers.
	uniq := fmt.Sprintf("%010x", time.Now().UnixNano()%0xffffffffff)
	uid := "aaaaaaaa-aaaa-4aaa-8aaa-aa" + uniq
	base := uint64(0xB000000 + time.Now().Unix()%0xffffff)
	lsn1 := fmt.Sprintf("0/%X", base)
	lsn2 := fmt.Sprintf("0/%X", base+1)
	lsn3 := fmt.Sprintf("0/%X", base+2)
	txn := fmt.Sprintf("live-%d", time.Now().UnixNano())
	// Clean slate for the live row.
	_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, uid)
	e := &CDCEvent{WorkloadID: "cloudshop", SourceDB: "cloudshop", Table: "users",
		Op: "c", PK: uid, LSN: lsn1, SourceCommitMs: 1700000000000,
		TxnID: txn + "-1", SchemaVersion: 1,
		After: map[string]any{"id": uid, "email": "live@example.com"}}
	applied, err := ps.ApplyAtomically(e)
	if err != nil || !applied {
		t.Fatalf("live insert: applied=%v err=%v", applied, err)
	}
	var email string
	if err := db.QueryRow(`SELECT email FROM users WHERE id = $1`, uid).Scan(&email); err != nil {
		t.Fatalf("target row missing: %v", err)
	}
	if email != "live@example.com" {
		t.Fatalf("email: %q", email)
	}
	// Replay dedupes.
	applied, err = ps.ApplyAtomically(e)
	if err != nil || applied {
		t.Fatalf("live replay: applied=%v err=%v", applied, err)
	}
	// Update propagates (new LSN == new event identity).
	u := &CDCEvent{WorkloadID: "cloudshop", SourceDB: "cloudshop", Table: "users",
		Op: "u", PK: uid, LSN: lsn2, SourceCommitMs: 1700000000001,
		TxnID: txn + "-2", SchemaVersion: 1,
		After: map[string]any{"id": uid, "email": "live2@example.com"}}
	if _, err := ps.ApplyAtomically(u); err != nil {
		t.Fatalf("live update: %v", err)
	}
	if err := db.QueryRow(`SELECT email FROM users WHERE id = $1`, uid).Scan(&email); err != nil || email != "live2@example.com" {
		t.Fatalf("update not reflected: %q %v", email, err)
	}
	// Delete propagates.
	d := &CDCEvent{WorkloadID: "cloudshop", SourceDB: "cloudshop", Table: "users",
		Op: "d", PK: uid, LSN: lsn3, SourceCommitMs: 1700000000002,
		TxnID: txn + "-3", SchemaVersion: 1,
		Before: map[string]any{"id": uid, "email": "live2@example.com"}}
	if _, err := ps.ApplyAtomically(d); err != nil {
		t.Fatalf("live delete: %v", err)
	}
	// Cleanup.
	if _, err := db.Exec(`DELETE FROM users WHERE id = $1`, uid); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cdc_applied_events WHERE event_id = $1`, e.EventID()).Scan(&n); err != nil || n != 1 {
		t.Fatalf("dedupe marker: n=%d err=%v", n, err)
	}
}
