// Store abstraction: MemStore (Phase 1 local default) or PGStore when
// DATABASE_URL is set. Both enforce identical idempotency semantics:
// same key + same hash -> replay; same key + different hash -> conflict.
package main

// Product mirrors the products table.
type Product struct {
	ID         string `json:"id"`
	SKU        string `json:"sku"`
	Name       string `json:"name"`
	PriceCents int64  `json:"price_cents"`
}

// Order mirrors the orders table.
type Order struct {
	ID         string `json:"id"`
	UserID     string `json:"user_id"`
	Status     string `json:"status"`
	TotalCents int64  `json:"total_cents"`
}

// IdemResult is a stored idempotent response.
type IdemResult struct {
	RequestHash string
	Response    []byte
	Status      int
	Found       bool
}

// Store is the persistence boundary for the API handlers.
type Store interface {
	GetProduct(id string) (Product, bool)
	ListProducts() []Product
	SeedProduct(p Product)
	GetOrder(id string) (Order, bool)
	CreateOrder(o Order) error
	LookupIdem(scope, key string) IdemResult
	SaveIdem(scope, key, hash string, resp []byte, status int)
	Ping() error
	Close() error
}
