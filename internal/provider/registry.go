package provider

import (
	"log/slog"
	"sort"
	"sync"
)

// Registry is a thread-safe registry of GPU cloud providers.
// It stores providers by name and supports concurrent reads.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry creates a new empty provider registry.
func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]Provider),
	}
}

// Register adds a provider to the registry. If a provider with the same name
// already exists, it is replaced (allows re-registration during config reload).
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.Name()] = p
	slog.Info("provider registered", "name", p.Name())
}

// Get returns the provider with the given name and a boolean indicating
// whether it was found.
func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// All returns a slice copy of all registered providers.
// Order is not guaranteed; callers should sort if needed.
func (r *Registry) All() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Provider, 0, len(r.providers))
	for _, p := range r.providers {
		result = append(result, p)
	}
	return result
}

// Names returns a sorted list of all registered provider names.
// Useful for logging at startup.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
