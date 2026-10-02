// PGStore: Postgres implementation of Store (used when DATABASE_URL is set).
// Schema: workloads/cloudshop/migrations/001_init.sql. Connections use
// sslmode=require in cloud; local compose may use sslmode=disable via URL.
package main

import (
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"
)

type PGStore struct {
	db *sql.DB
}

func NewPGStore(url string) (*PGStore, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pg ping: %w", err)
	}
	// H-3: durable admin state (single-row fact; ensured idempotently so
	// pre-existing volumes upgrade without manual migration).
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS admin_state (
		singleton INT PRIMARY KEY DEFAULT 1 CHECK (singleton = 1),
		write_ownership TEXT NOT NULL,
		quiesced BOOLEAN NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("pg ensure admin_state: %w", err)
	}
	return &PGStore{db: db}, nil
}

// LoadAdminState reads the durable admin fact. ok=false when no transfer
// was ever persisted (fresh backend).
func (p *PGStore) LoadAdminState() (adminState, bool, error) {
	var st adminState
	err := p.db.QueryRow(`SELECT write_ownership, quiesced FROM admin_state WHERE singleton = 1`).
		Scan(&st.WriteOwnership, &st.Quiesced)
	if err == sql.ErrNoRows {
		return adminState{}, false, nil
	}
	if err != nil {
		return adminState{}, false, err
	}
	if st.WriteOwnership != "aws" && st.WriteOwnership != "azure" {
		return adminState{}, false, fmt.Errorf("corrupt admin_state row: ownership %q", st.WriteOwnership)
	}
	return st, true, nil
}

// SaveAdminState upserts the durable admin fact (the persist half of
// persist-then-swap).
func (p *PGStore) SaveAdminState(owner string, quiesced bool) error {
	if owner != "aws" && owner != "azure" {
		return fmt.Errorf("refusing to persist invalid ownership %q", owner)
	}
	_, err := p.db.Exec(`INSERT INTO admin_state (singleton, write_ownership, quiesced, updated_at)
		VALUES (1, $1, $2, now())
		ON CONFLICT (singleton) DO UPDATE SET write_ownership = EXCLUDED.write_ownership,
			quiesced = EXCLUDED.quiesced, updated_at = now()`, owner, quiesced)
	return err
}

func (p *PGStore) GetProduct(id string) (Product, bool) {
	var pr Product
	err := p.db.QueryRow(`SELECT id::text, sku, name, price_cents FROM products WHERE id = $1`, id).
		Scan(&pr.ID, &pr.SKU, &pr.Name, &pr.PriceCents)
	if err != nil {
		return Product{}, false
	}
	return pr, true
}

func (p *PGStore) ListProducts() []Product {
	rows, err := p.db.Query(`SELECT id::text, sku, name, price_cents FROM products ORDER BY created_at`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var items []Product
	for rows.Next() {
		var pr Product
		if err := rows.Scan(&pr.ID, &pr.SKU, &pr.Name, &pr.PriceCents); err != nil {
			continue
		}
		items = append(items, pr)
	}
	return items
}

func (p *PGStore) SeedProduct(pr Product) {
	var count int
	if err := p.db.QueryRow(`SELECT count(*) FROM products`).Scan(&count); err != nil || count > 0 {
		return
	}
	_, _ = p.db.Exec(`INSERT INTO products (id, sku, name, price_cents) VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, pr.ID, pr.SKU, pr.Name, pr.PriceCents)
}

func (p *PGStore) GetOrder(id string) (Order, bool) {
	var o Order
	err := p.db.QueryRow(`SELECT id::text, user_id::text, status, total_cents FROM orders WHERE id = $1`, id).
		Scan(&o.ID, &o.UserID, &o.Status, &o.TotalCents)
	if err != nil {
		return Order{}, false
	}
	return o, true
}

func (p *PGStore) CreateOrder(o Order) error {
	_, err := p.db.Exec(`INSERT INTO orders (id, user_id, status, total_cents) VALUES ($1, $2, $3, $4)`,
		o.ID, o.UserID, o.Status, o.TotalCents)
	return err
}

func (p *PGStore) LookupIdem(scope, key string) IdemResult {
	var r IdemResult
	err := p.db.QueryRow(`SELECT request_hash, response_payload, 201 FROM idempotency_keys
		WHERE scope = $1 AND key = $2 AND expires_at > now()`, scope, key).
		Scan(&r.RequestHash, &r.Response, &r.Status)
	if err != nil {
		return IdemResult{}
	}
	r.Found = true
	return r
}

func (p *PGStore) SaveIdem(scope, key, hash string, resp []byte, status int) {
	// status is informational (v1 always replays stored payload with its status);
	// insert-or-keep-first makes concurrent double-submit safe (first wins).
	_, _ = p.db.Exec(`INSERT INTO idempotency_keys (key, scope, request_hash, response_payload)
		VALUES ($1, $2, $3, $4) ON CONFLICT (scope, key) DO NOTHING`, key, scope, hash, string(resp))
}

func (p *PGStore) Ping() error { return p.db.Ping() }
func (p *PGStore) Close() error { return p.db.Close() }
