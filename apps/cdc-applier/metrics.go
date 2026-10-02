// Operational counters for the applier. Lightweight by design: no metrics
// server in the repo yet, so this exposes structured state suitable for later
// OTel/Prometheus wiring (names mirror OBSERVABILITY.md replication metrics).
//
// Never records credentials, payloads, or PII — only counts, LSNs, and lag.
package cdc

import (
	"sync"
	"time"
)

// Counters tracks events received/applied/duplicates/failures plus the current
// lag gauge and checkpoint position/age.
type Counters struct {
	mu             sync.Mutex
	received       uint64
	applied        uint64
	duplicates     uint64
	failures       uint64
	lagSeconds     int64
	checkpointLSN  string
	checkpointAt   time.Time
	lastEventID    string
	consumerRestarts uint64
}

func NewCounters() *Counters { return &Counters{} }

func (c *Counters) IncReceived()  { c.mu.Lock(); c.received++; c.mu.Unlock() }
func (c *Counters) IncApplied()   { c.mu.Lock(); c.applied++; c.mu.Unlock() }
func (c *Counters) IncDuplicate() { c.mu.Lock(); c.duplicates++; c.mu.Unlock() }
func (c *Counters) IncFailure()   { c.mu.Lock(); c.failures++; c.mu.Unlock() }

func (c *Counters) IncRestart() { c.mu.Lock(); c.consumerRestarts++; c.mu.Unlock() }

func (c *Counters) SetCheckpoint(lsn string) {
	c.mu.Lock()
	c.checkpointLSN = lsn
	c.checkpointAt = time.Now()
	c.mu.Unlock()
}

// ObserveLag records cdc_lag_seconds = observeUnix - commitUnix (clamped >= 0).
func (c *Counters) ObserveLag(commitUnix, observeUnix int64) int64 {
	lag := LagSeconds(commitUnix, observeUnix)
	if lag < 0 {
		lag = 0
	}
	c.mu.Lock()
	c.lagSeconds = lag
	c.mu.Unlock()
	return lag
}

// Snapshot is the inspectable operational state (logs/status endpoint).
type Snapshot struct {
	Received         uint64 `json:"events_received"`
	Applied          uint64 `json:"events_applied"`
	Duplicates       uint64 `json:"events_duplicates"`
	Failures         uint64 `json:"events_failures"`
	LagSeconds       int64  `json:"cdc_lag_seconds"`
	CheckpointLSN    string `json:"checkpoint_lsn"`
	CheckpointAgeSec int64  `json:"checkpoint_age_seconds"`
	ConsumerRestarts uint64 `json:"consumer_restarts"`
}

func (c *Counters) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	age := int64(0)
	if !c.checkpointAt.IsZero() {
		age = int64(time.Since(c.checkpointAt).Seconds())
		if age < 0 {
			age = 0
		}
	}
	return Snapshot{
		Received: c.received, Applied: c.applied,
		Duplicates: c.duplicates, Failures: c.failures,
		LagSeconds: c.lagSeconds, CheckpointLSN: c.checkpointLSN,
		CheckpointAgeSec: age, ConsumerRestarts: c.consumerRestarts,
	}
}
