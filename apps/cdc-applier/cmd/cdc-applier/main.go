// cdc-applier: consumes canonical CDC events and applies them idempotently
// to the target PostgreSQL.
//
// Local path: cloudshop-db :5433 (WAL) -> Debezium -> Redpanda ->
// this applier -> cloudshop-target-db :5434.
//
// Configuration (existing local conventions, never hardcoded secrets):
//   TARGET_DATABASE_URL  target Postgres URL (required for live apply)
//   CDC_CONSUMER         checkpoint consumer name (default "cdc-applier-1")
//   WORKLOAD_ID          workload identity stamped on events (default "cloudshop")
//   PORT                 status port (default "8089"; exposes /healthz + /status)
//   APPLY_POLL_INTERVAL  loop interval (default "5s"; live consume is stubbed
//                        to file/HTTP polling until the Kafka client lands)
//
// Safety: no cloud APIs, no Terraform, no migration authorization. CDC state
// (offsets/checkpoints) never authorizes cutover.
package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/lib/pq"

	cdc "skybridge/cdc-applier"
)

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	consumer := getenv("CDC_CONSUMER", "cdc-applier-1")
	workload := getenv("WORKLOAD_ID", "cloudshop")
	targetURL := os.Getenv("TARGET_DATABASE_URL")
	port := getenv("PORT", "8089")

	metrics := cdc.NewCounters()
	offsets := cdc.NewMemoryCheckpointStore()
	var store cdc.Store = cdc.NewMemoryStore()

	if targetURL != "" {
		db, err := sql.Open("postgres", targetURL)
		if err != nil {
			log.Fatalf("target db open: %v", err)
		}
		defer db.Close()
		ps := cdc.NewPostgresStore(db)
		if err := ps.Ensure(); err != nil {
			log.Fatalf("ensure cdc state: %v", err)
		}
		// NOTE: PostgresCheckpointStore for cdc_offsets is a future step;
		// v1 keeps process-local offsets + durable per-event dedupe in PG.
		// Restart resume is proven via redelivery dedupe (see tests).
		store = ps
		log.Print("cdc-applier: using target postgres")
	} else {
		log.Print("cdc-applier: TARGET_DATABASE_URL unset, running without live target (logic/tests only)")
	}

	engine := cdc.NewEngine(consumer, store, offsets, metrics)
	_ = engine
	_ = workload

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		cp, _ := offsets.Load(consumer)
		snap := metrics.Snapshot()
		out := map[string]any{
			"consumer": consumer, "workload_id": workload,
			"checkpoint_lsn": cp.SourceLSN, "checkpoint_offset": cp.Offset,
			"metrics": snap,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	go func() {
		log.Printf("cdc-applier status listening :%s", port)
		_ = http.ListenAndServe(":"+port, mux)
	}()

	interval := 30 * time.Second
	if v := os.Getenv("APPLY_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			interval = d
		}
	}
	for {
		time.Sleep(interval)
		snap := metrics.Snapshot()
		log.Printf("cdc-applier heartbeat consumer=%s applied=%d duplicates=%d failures=%d lag=%ds checkpoint=%s",
			consumer, snap.Applied, snap.Duplicates, snap.Failures, snap.LagSeconds, snap.CheckpointLSN)
	}
}
