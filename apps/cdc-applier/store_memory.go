// In-memory Store for hermetic unit tests. Mirrors PostgresStore semantics:
// row mutation + dedupe marker are atomic under a single mutex; txn marker is
// recorded but never consulted for skip decisions.
package cdc

import (
	"fmt"
	"sync"
)

type memRow struct {
	table string
	data  map[string]any
}

// MemoryStore implements Store without a database.
type MemoryStore struct {
	mu      sync.Mutex
	rows    map[string]map[string]memRow // table -> pk -> row
	applied map[string]bool              // event_id -> true
	txns    map[string]bool              // txn observability marker
	// failNext forces the next ApplyAtomically to fail after validation,
	// exercising rollback (no row, no dedupe marker).
	failNext error
	// failTable forces failures for a specific table (e.g. transient DB error).
	failTable map[string]error
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		rows: map[string]map[string]memRow{
			"users": {}, "products": {}, "orders": {}, "order_items": {},
		},
		applied:   map[string]bool{},
		txns:      map[string]bool{},
		failTable: map[string]error{},
	}
}

func (m *MemoryStore) HasApplied(eventID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applied[eventID], nil
}

func (m *MemoryStore) InjectFailure(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failNext = err
}

func (m *MemoryStore) InjectTableFailure(table string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failTable[table] = err
}

func copyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (m *MemoryStore) ApplyAtomically(e *CDCEvent) (bool, error) {
	if err := e.Validate(); err != nil {
		return false, err
	}
	// Validate SQL shape too: surfaces schema errors without a database.
	if _, _, err := BuildApplySQL(e); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.applied[e.EventID()] {
		return false, nil
	}
	if m.failNext != nil {
		err := m.failNext
		m.failNext = nil
		return false, err
	}
	if err, ok := m.failTable[e.Table]; ok && err != nil {
		return false, err
	}
	tbl, ok := m.rows[e.Table]
	if !ok {
		return false, fmt.Errorf("unsupported table %q", e.Table)
	}
	row := rowForUpdate(e)
	switch e.Op {
	case "c":
		if _, exists := tbl[e.PK]; !exists {
			tbl[e.PK] = memRow{table: e.Table, data: copyMap(row)}
		}
	case "r":
		tbl[e.PK] = memRow{table: e.Table, data: copyMap(row)}
	case "u":
		tbl[e.PK] = memRow{table: e.Table, data: copyMap(row)} // upsert tolerant
	case "d":
		delete(tbl, e.PK)
	default:
		return false, fmt.Errorf("unknown op %q", e.Op)
	}
	m.applied[e.EventID()] = true
	if e.TxnID != "" {
		m.txns[e.TxnID] = true
	}
	return true, nil
}

// Get returns a copy of a stored row for assertions.
func (m *MemoryStore) Get(table, pk string) (map[string]any, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tbl, ok := m.rows[table]
	if !ok {
		return nil, false
	}
	r, ok := tbl[pk]
	if !ok {
		return nil, false
	}
	return copyMap(r.data), true
}

func (m *MemoryStore) AppliedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.applied)
}
