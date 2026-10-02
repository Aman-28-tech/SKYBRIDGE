// CloudShop worker v1 — duplicate-safe job consumer stub.
// Job envelope: {job_id, type: order.confirm|export.generate, payload, attempts}.
// Dedupes via applied_jobs table (in-memory Phase 1; Postgres in cloud).
// Queue drain contract: producers pause -> depth<=10 sustained 60s -> cutover.
package main

import (
	"log"
	"os"
	"sync"
	"time"
)

type Job struct {
	JobID   string `json:"job_id"`
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

var applied = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

func handle(j Job) {
	applied.Lock()
	if applied.m[j.JobID] {
		applied.Unlock()
		log.Printf("skip duplicate job_id=%s", j.JobID)
		return
	}
	applied.m[j.JobID] = true
	applied.Unlock()
	// Simulate work: order confirmation / export generation.
	log.Printf("applied job_id=%s type=%s", j.JobID, j.Type)
}

func main() {
	log.Printf("cloudshop worker starting (queue=%s)", os.Getenv("QUEUE_URL"))
	// Phase 1 local: idle loop; real broker subscription lands with Redpanda consumer in Phase 7.
	for {
		time.Sleep(30 * time.Second)
		log.Print("worker heartbeat: no pending jobs (Phase 1 stub)")
	}
}
