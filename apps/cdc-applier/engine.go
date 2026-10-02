// Engine: ordered, idempotent consumption of the CDC stream.
//
// v1 ordering guarantee (documented, tested):
//   - Within one Redpanda partition, records arrive in source commit order and
//     are applied in LSN order.
//   - The same transaction's events share a TxnID and consecutive LSNs /
//     data_collection_order; v1 applies them sequentially, one SQL txn each.
//     Cross-event atomicity is NOT provided (stated explicitly).
//   - A redelivered event (same EventID) is a duplicate: skipped, checkpoint
//     re-asserted forward-only.
//   - An event whose LSN is older than the checkpoint but was never applied is
//     a late/out-of-order arrival: v1 fails it explicitly (ErrOutOfOrder) for
//     retry/inspection rather than silently reordering history.
//   - Arrival time never influences ordering or identity.
package cdc

import (
	"errors"
	"fmt"
)

// ErrOutOfOrder signals a late event that cannot be safely applied without
// risking history rewrite. Callers must retry/park it, not skip silently.
var ErrOutOfOrder = errors.New("CDC event out of order: LSN older than checkpoint and not previously applied")

// Engine wires Store + CheckpointStore + Counters.
type Engine struct {
	Consumer string
	Store    Store
	Offsets  CheckpointStore
	Metrics  *Counters
}

// NewEngine builds an engine over the given durable stores.
func NewEngine(consumer string, s Store, o CheckpointStore, m *Counters) *Engine {
	if m == nil {
		m = NewCounters()
	}
	return &Engine{Consumer: consumer, Store: s, Offsets: o, Metrics: m}
}

// Apply consumes one canonical event. Returns applied=true when the row
// mutation happened, false for duplicates. Checkpoint advances only on
// success (or duplicate re-assertion), never past unapplied work.
func (en *Engine) Apply(e *CDCEvent, sourceCommitUnix, observeUnix int64) (bool, error) {
	if err := e.Validate(); err != nil {
		en.Metrics.IncFailure()
		return false, err
	}
	en.Metrics.IncReceived()

	already, err := en.Store.HasApplied(e.EventID())
	if err != nil {
		en.Metrics.IncFailure()
		return false, fmt.Errorf("dedupe lookup: %w", err)
	}
	cp, err := en.Offsets.Load(en.Consumer)
	if err != nil {
		en.Metrics.IncFailure()
		return false, fmt.Errorf("checkpoint load: %w", err)
	}
	if already {
		en.Metrics.IncDuplicate()
		// Re-assert checkpoint forward-only (covers apply-ok/checkpoint-fail).
		_ = en.Offsets.Save(Checkpoint{Consumer: en.Consumer, SourceLSN: e.LSN, Offset: cp.Offset + 0})
		en.Metrics.ObserveLag(sourceCommitUnix, observeUnix)
		return false, nil
	}
	if cp.SourceLSN != "" {
		cmp, err := CompareLSN(e.LSN, cp.SourceLSN)
		if err != nil {
			en.Metrics.IncFailure()
			return false, err
		}
		if cmp < 0 {
			// Older than everything applied, never seen: unsafe to apply.
			en.Metrics.IncFailure()
			return false, fmt.Errorf("%w: event %s lsn %s behind checkpoint %s",
				ErrOutOfOrder, e.EventID(), e.LSN, cp.SourceLSN)
		}
	}
	applied, err := en.Store.ApplyAtomically(e)
	if err != nil {
		en.Metrics.IncFailure()
		return false, err
	}
	if !applied {
		// Lost race with a concurrent applier: treat as duplicate.
		en.Metrics.IncDuplicate()
		return false, nil
	}
	en.Metrics.IncApplied()
	// Checkpoint advances only after the atomic apply committed.
	next := Checkpoint{Consumer: en.Consumer, SourceLSN: e.LSN, Offset: cp.Offset + 1}
	if err := en.Offsets.Save(next); err != nil {
		// Apply committed but checkpoint failed: safe — replay dedupes.
		en.Metrics.IncFailure()
		return true, fmt.Errorf("apply committed but checkpoint failed (replay will dedupe): %w", err)
	}
	en.Metrics.SetCheckpoint(e.LSN)
	en.Metrics.ObserveLag(sourceCommitUnix, observeUnix)
	return true, nil
}
