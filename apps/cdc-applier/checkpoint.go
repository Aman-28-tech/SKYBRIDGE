// LSN handling and durable checkpointing.
//
// LSN authority: PostgreSQL log sequence numbers ("0/16B1978") order the
// source stream. Arrival time is never ordering. Checkpoint = the greatest
// LSN known to be safely applied for a consumer; it advances monotonically
// and only after the event's atomic apply commits.
//
// At-least-once reasoning:
//   - apply ok + checkpoint ok            -> advance, exactly-once effect.
//   - apply ok + checkpoint write fails   -> replay redelivers; dedupe skips
//     the row mutation, checkpoint advances on the replay. No double effect.
//   - apply fails                         -> checkpoint untouched; retry.
//   - duplicate after restart             -> dedupe hit; checkpoint re-asserted
//     forward-only (never rewound).
package cdc

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ParseLSN converts "0/16B1978" to a comparable uint64 (hi<<32 | lo).
func ParseLSN(lsn string) (uint64, error) {
	parts := strings.Split(lsn, "/")
	if len(parts) != 2 {
		return 0, fmt.Errorf("malformed lsn %q", lsn)
	}
	hi, err := strconv.ParseUint(parts[0], 16, 32)
	if err != nil {
		return 0, fmt.Errorf("malformed lsn hi %q: %w", lsn, err)
	}
	lo, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil {
		return 0, fmt.Errorf("malformed lsn lo %q: %w", lsn, err)
	}
	return hi<<32 | lo, nil
}

// CompareLSN returns -1/0/+1. Malformed LSNs are errors, never silent order.
func CompareLSN(a, b string) (int, error) {
	if a == b {
		return 0, nil
	}
	av, err := ParseLSN(a)
	if err != nil {
		return 0, err
	}
	bv, err := ParseLSN(b)
	if err != nil {
		return 0, err
	}
	switch {
	case av < bv:
		return -1, nil
	case av > bv:
		return 1, nil
	default:
		return 0, nil
	}
}

// Checkpoint is the durable source position for one consumer.
// It mirrors the cdc_offsets row: (consumer, source_lsn, offset).
type Checkpoint struct {
	Consumer  string
	SourceLSN string
	Offset    int64
	UpdatedAt time.Time
}

// CheckpointStore persists checkpoints durably.
type CheckpointStore interface {
	Load(consumer string) (Checkpoint, error)
	// Save advances forward-only: saving an older LSN is a no-op success.
	Save(cp Checkpoint) error
}

// MemoryCheckpointStore is the hermetic implementation (and documents the
// forward-only rule enforced by the SQL implementation).
type MemoryCheckpointStore struct {
	mu  sync.Mutex
	cur map[string]Checkpoint
}

func NewMemoryCheckpointStore() *MemoryCheckpointStore {
	return &MemoryCheckpointStore{cur: map[string]Checkpoint{}}
}

func (m *MemoryCheckpointStore) Load(consumer string) (Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cp, ok := m.cur[consumer]; ok {
		return cp, nil
	}
	return Checkpoint{Consumer: consumer}, nil
}

func (m *MemoryCheckpointStore) Save(cp Checkpoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.cur[cp.Consumer]
	if !ok || cur.SourceLSN == "" {
		cp.UpdatedAt = time.Now()
		m.cur[cp.Consumer] = cp
		return nil
	}
	cmp, err := CompareLSN(cp.SourceLSN, cur.SourceLSN)
	if err != nil {
		return err
	}
	if cmp <= 0 {
		return nil // never rewind past applied work
	}
	cp.UpdatedAt = time.Now()
	m.cur[cp.Consumer] = cp
	return nil
}
