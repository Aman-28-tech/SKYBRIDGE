// Apply engine: idempotent, transactional application of canonical CDC events
// to the CloudShop target schema.
//
// Correctness contract (Option A):
//   - cdc_applied_events(event_id PK) is AUTHORITATIVE per-event dedupe.
//   - cdc_applied_txn(txn_id) is an observability marker only; it is written
//     after an event applies but NEVER read to skip an event. This is what
//     makes T1={E1,E2,E3} safe: E1 marking txn T1 must not skip E2/E3.
//   - One SQL transaction per event (no cross-event atomicity claimed).
//   - Failure at any point rolls back the whole event (no partial apply).
//   - At-least-once + idempotent apply == replay-safe.
//
// Table scope: users, products, orders, order_items only. Redis, API
// idempotency_keys, worker bookkeeping are never replicated.
package cdc

import (
	"database/sql"
	"fmt"
	"strings"
)

// Store is the durable application boundary. Implementations must make the
// row mutation + dedupe marker atomic (single SQL txn or equivalent lock).
type Store interface {
	// HasApplied reports whether eventID was already applied.
	HasApplied(eventID string) (bool, error)
	// ApplyAtomically applies the row change and records eventID in one
	// atomic unit. Returns applied=false when eventID was already present
	// (duplicate, no row mutation). Any error rolls back everything.
	ApplyAtomically(e *CDCEvent) (applied bool, err error)
}

// ---- SQL statement builders (explicit per-table, CloudShop schema) ----

func strCol(m map[string]any, k string) (string, error) {
	v, ok := m[k]
	if !ok || v == nil {
		return "", fmt.Errorf("missing column %q", k)
	}
	s := stringify(v)
	if s == "" {
		return "", fmt.Errorf("empty column %q", k)
	}
	return s, nil
}

func intCol(m map[string]any, k string) (int64, error) {
	v, ok := m[k]
	if !ok || v == nil {
		return 0, fmt.Errorf("missing column %q", k)
	}
	switch t := v.(type) {
	case float64:
		return int64(t), nil
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	default:
		return 0, fmt.Errorf("column %q not numeric (%T)", k, v)
	}
}

// rowForUpdate returns the payload to apply: after preferred, before for deletes.
func rowForUpdate(e *CDCEvent) map[string]any {
	if len(e.After) > 0 {
		return e.After
	}
	return e.Before
}

// ApplySQL describes one atomic application. Used by PostgresStore and by
// tests to assert the exact schema touched (no generic SQL hiding errors).
type ApplySQL struct {
	Statement string
	Args      []any
}

// BuildApplySQL returns the per-table statement for event e.
// c: INSERT ... ON CONFLICT DO NOTHING (duplicate-safe).
// r: snapshot read -> upsert (INSERT ... ON CONFLICT DO UPDATE).
// u: UPDATE, falling back to INSERT when the row is absent (snapshot/stream
// overlap tolerance; FK violations still error and retry, never corrupt).
// d: DELETE WHERE id (idempotent; 0 rows is success).
func BuildApplySQL(e *CDCEvent) (primary ApplySQL, fallback *ApplySQL, err error) {
	row := rowForUpdate(e)
	switch e.Table {
	case "users":
		email, err := strCol(row, "email")
		if err != nil {
			return ApplySQL{}, nil, err
		}
		switch e.Op {
		case "c":
			return ApplySQL{"INSERT INTO users (id, email) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING", []any{e.PK, email}}, nil, nil
		case "r":
			return ApplySQL{"INSERT INTO users (id, email) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET email = EXCLUDED.email", []any{e.PK, email}}, nil, nil
		case "u":
			return ApplySQL{"UPDATE users SET email = $2 WHERE id = $1", []any{e.PK, email}},
				&ApplySQL{"INSERT INTO users (id, email) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING", []any{e.PK, email}}, nil
		case "d":
			return ApplySQL{"DELETE FROM users WHERE id = $1", []any{e.PK}}, nil, nil
		}
	case "products":
		sku, err := strCol(row, "sku")
		if err != nil {
			return ApplySQL{}, nil, err
		}
		name, err := strCol(row, "name")
		if err != nil {
			return ApplySQL{}, nil, err
		}
		price, err := intCol(row, "price_cents")
		if err != nil {
			return ApplySQL{}, nil, err
		}
		switch e.Op {
		case "c":
			return ApplySQL{"INSERT INTO products (id, sku, name, price_cents) VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO NOTHING", []any{e.PK, sku, name, price}}, nil, nil
		case "r":
			return ApplySQL{"INSERT INTO products (id, sku, name, price_cents) VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO UPDATE SET sku = EXCLUDED.sku, name = EXCLUDED.name, price_cents = EXCLUDED.price_cents", []any{e.PK, sku, name, price}}, nil, nil
		case "u":
			return ApplySQL{"UPDATE products SET sku = $2, name = $3, price_cents = $4 WHERE id = $1", []any{e.PK, sku, name, price}},
				&ApplySQL{"INSERT INTO products (id, sku, name, price_cents) VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO NOTHING", []any{e.PK, sku, name, price}}, nil
		case "d":
			return ApplySQL{"DELETE FROM products WHERE id = $1", []any{e.PK}}, nil, nil
		}
	case "orders":
		userID, err := strCol(row, "user_id")
		if err != nil {
			return ApplySQL{}, nil, err
		}
		status, err := strCol(row, "status")
		if err != nil {
			return ApplySQL{}, nil, err
		}
		if status != "pending" && status != "confirmed" && status != "failed" {
			return ApplySQL{}, nil, fmt.Errorf("invalid order status %q", status)
		}
		total, err := intCol(row, "total_cents")
		if err != nil {
			return ApplySQL{}, nil, err
		}
		switch e.Op {
		case "c":
			return ApplySQL{"INSERT INTO orders (id, user_id, status, total_cents) VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO NOTHING", []any{e.PK, userID, status, total}}, nil, nil
		case "r":
			return ApplySQL{"INSERT INTO orders (id, user_id, status, total_cents) VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO UPDATE SET user_id = EXCLUDED.user_id, status = EXCLUDED.status, total_cents = EXCLUDED.total_cents", []any{e.PK, userID, status, total}}, nil, nil
		case "u":
			return ApplySQL{"UPDATE orders SET user_id = $2, status = $3, total_cents = $4 WHERE id = $1", []any{e.PK, userID, status, total}},
				&ApplySQL{"INSERT INTO orders (id, user_id, status, total_cents) VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO NOTHING", []any{e.PK, userID, status, total}}, nil
		case "d":
			return ApplySQL{"DELETE FROM orders WHERE id = $1", []any{e.PK}}, nil, nil
		}
	case "order_items":
		orderID, err := strCol(row, "order_id")
		if err != nil {
			return ApplySQL{}, nil, err
		}
		productID, err := strCol(row, "product_id")
		if err != nil {
			return ApplySQL{}, nil, err
		}
		qty, err := intCol(row, "quantity")
		if err != nil {
			return ApplySQL{}, nil, err
		}
		if qty <= 0 {
			return ApplySQL{}, nil, fmt.Errorf("invalid quantity %d", qty)
		}
		unit, err := intCol(row, "unit_price_cents")
		if err != nil {
			return ApplySQL{}, nil, err
		}
		switch e.Op {
		case "c":
			return ApplySQL{"INSERT INTO order_items (id, order_id, product_id, quantity, unit_price_cents) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (id) DO NOTHING", []any{e.PK, orderID, productID, qty, unit}}, nil, nil
		case "r":
			return ApplySQL{"INSERT INTO order_items (id, order_id, product_id, quantity, unit_price_cents) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (id) DO UPDATE SET order_id = EXCLUDED.order_id, product_id = EXCLUDED.product_id, quantity = EXCLUDED.quantity, unit_price_cents = EXCLUDED.unit_price_cents", []any{e.PK, orderID, productID, qty, unit}}, nil, nil
		case "u":
			return ApplySQL{"UPDATE order_items SET order_id = $2, product_id = $3, quantity = $4, unit_price_cents = $5 WHERE id = $1", []any{e.PK, orderID, productID, qty, unit}},
				&ApplySQL{"INSERT INTO order_items (id, order_id, product_id, quantity, unit_price_cents) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (id) DO NOTHING", []any{e.PK, orderID, productID, qty, unit}}, nil
		case "d":
			return ApplySQL{"DELETE FROM order_items WHERE id = $1", []any{e.PK}}, nil, nil
		}
	}
	return ApplySQL{}, nil, fmt.Errorf("unsupported table %q", e.Table)
}

// ---- PostgresStore: real SQL implementation ----

// EnsureCDCStateSQL creates the dedupe/offset tables if absent. The applier
// runs this on startup so existing volumes (initialized with 001 only) and
// fresh volumes converge without manual steps.
const EnsureCDCStateSQL = `
CREATE TABLE IF NOT EXISTS cdc_applied_events (
  event_id TEXT PRIMARY KEY,
  txn_id TEXT NOT NULL DEFAULT '',
  tbl TEXT NOT NULL DEFAULT '',
  source_lsn TEXT NOT NULL DEFAULT '',
  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS cdc_applied_txn (
  txn_id TEXT PRIMARY KEY,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS cdc_offsets (
  consumer TEXT PRIMARY KEY,
  source_lsn TEXT NOT NULL DEFAULT '',
  "offset" BIGINT NOT NULL DEFAULT 0,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`

// PostgresStore applies against a real target database.
type PostgresStore struct {
	db *sql.DB
}

func NewPostgresStore(db *sql.DB) *PostgresStore { return &PostgresStore{db: db} }

func (s *PostgresStore) Ensure() error {
	_, err := s.db.Exec(EnsureCDCStateSQL)
	return err
}

func (s *PostgresStore) HasApplied(eventID string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM cdc_applied_events WHERE event_id = $1`, eventID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ApplyAtomically runs row mutation + dedupe insert + txn marker in one SQL
// transaction. Any error rolls back everything (no partial apply).
func (s *PostgresStore) ApplyAtomically(e *CDCEvent) (bool, error) {
	if err := e.Validate(); err != nil {
		return false, err
	}
	primary, fallback, err := BuildApplySQL(e)
	if err != nil {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	rollback := func() { _ = tx.Rollback() }
	// Dedupe guard inside the txn: concurrent appliers cannot double-apply
	// because event_id is the primary key.
	var one int
	err = tx.QueryRow(`SELECT 1 FROM cdc_applied_events WHERE event_id = $1`, e.EventID()).Scan(&one)
	if err == nil {
		_ = tx.Rollback()
		return false, nil
	}
	if err != sql.ErrNoRows {
		rollback()
		return false, fmt.Errorf("dedupe lookup: %w", err)
	}
	res, err := tx.Exec(primary.Statement, primary.Args...)
	if err != nil {
		rollback()
		return false, fmt.Errorf("apply row: %w", err)
	}
	if fallback != nil {
		if n, _ := res.RowsAffected(); n == 0 {
			if _, err := tx.Exec(fallback.Statement, fallback.Args...); err != nil {
				rollback()
				return false, fmt.Errorf("apply fallback insert: %w", err)
			}
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO cdc_applied_events (event_id, txn_id, tbl, source_lsn) VALUES ($1, $2, $3, $4) ON CONFLICT (event_id) DO NOTHING`,
		e.EventID(), e.TxnID, e.Table, e.LSN); err != nil {
		rollback()
		return false, fmt.Errorf("dedupe insert: %w", err)
	}
	// Observability marker only. Never read for skip decisions.
	if e.TxnID != "" {
		_, _ = tx.Exec(`INSERT INTO cdc_applied_txn (txn_id) VALUES ($1) ON CONFLICT (txn_id) DO NOTHING`, e.TxnID)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// tableOfEvent is a helper for error messages.
func tableOfEvent(e *CDCEvent) string { return e.Table + ":" + e.PK }

// isMissingTableError detects a target missing the CloudShop schema.
func isMissingTableError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "relation")
}
