package provider

import "sync"

// ProviderMeta holds static metadata about a supported upstream provider.
type ProviderMeta struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Format    string   `json:"format"`
	BaseURL   string   `json:"base_url"`
	AuthTypes []string `json:"auth_types"`
}

// Registry is a thread-safe store of provider metadata, keyed by provider ID.
type Registry struct {
	mu    sync.RWMutex
	items map[string]ProviderMeta
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		items: make(map[string]ProviderMeta),
	}
}

// Register adds or replaces provider metadata.
func (r *Registry) Register(id string, meta ProviderMeta) {
	r.mu.Lock()
	defer r.mu.Unlock()
	meta.ID = id // ensure consistency
	r.items[id] = meta
}

// Get returns the metadata for a provider, or nil if not found.
func (r *Registry) Get(id string) *ProviderMeta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.items[id]
	if !ok {
		return nil
	}
	return &m
}

// All returns a snapshot of every registered provider.
func (r *Registry) All() map[string]ProviderMeta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]ProviderMeta, len(r.items))
	for k, v := range r.items {
		out[k] = v
	}
	return out
}

// Remove deletes a provider from the registry.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.items, id)
}

// Has reports whether a provider ID is registered.
func (r *Registry) Has(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.items[id]
	return ok
}
