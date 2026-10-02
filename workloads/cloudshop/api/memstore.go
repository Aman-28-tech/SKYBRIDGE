// MemStore: in-process implementation (Phase 1 local default).
package main

import "sync"

type MemStore struct {
	mu       sync.RWMutex
	products map[string]Product
	orders   map[string]Order
	idem     map[string]IdemResult // scope + "\x00" + key
}

func NewMemStore() *MemStore {
	return &MemStore{
		products: map[string]Product{},
		orders:   map[string]Order{},
		idem:     map[string]IdemResult{},
	}
}

func (m *MemStore) GetProduct(id string) (Product, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.products[id]
	return p, ok
}

func (m *MemStore) ListProducts() []Product {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]Product, 0, len(m.products))
	for _, p := range m.products {
		items = append(items, p)
	}
	return items
}

func (m *MemStore) SeedProduct(p Product) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.products) == 0 {
		m.products[p.ID] = p
	}
}

func (m *MemStore) GetOrder(id string) (Order, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.orders[id]
	return o, ok
}

func (m *MemStore) CreateOrder(o Order) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.orders[o.ID] = o
	return nil
}

func (m *MemStore) LookupIdem(scope, key string) IdemResult {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.idem[scope+"\x00"+key]
	r.Found = ok
	return r
}

func (m *MemStore) SaveIdem(scope, key, hash string, resp []byte, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.idem[scope+"\x00"+key] = IdemResult{RequestHash: hash, Response: resp, Status: status, Found: true}
}

func (m *MemStore) Ping() error { return nil }
func (m *MemStore) Close() error { return nil }
