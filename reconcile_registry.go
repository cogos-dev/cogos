// reconcile_registry.go
// Provider registry for the CogOS reconciliation framework.
// Maps resource type names to Reconcilable implementations.
//
// Usage:
//   RegisterProvider("discord", &DiscordProvider{})
//   provider, err := GetProvider("discord")

package main

import (
	"fmt"
	"sort"
	"sync"
)

var (
	providersMu sync.RWMutex
	providers   = make(map[string]Reconcilable)
)

// RegisterProvider adds a reconciliation provider to the global registry.
// Panics if a provider with the same name is already registered.
func RegisterProvider(name string, provider Reconcilable) {
	providersMu.Lock()
	defer providersMu.Unlock()
	if _, exists := providers[name]; exists {
		panic(fmt.Sprintf("reconcile: provider %q already registered", name))
	}
	providers[name] = provider
}

// GetProvider returns the provider for the given resource type.
func GetProvider(name string) (Reconcilable, error) {
	providersMu.RLock()
	defer providersMu.RUnlock()
	p, ok := providers[name]
	if !ok {
		return nil, fmt.Errorf("unknown resource type: %s (registered: %v)", name, ListProviders())
	}
	return p, nil
}

// ListProviders returns sorted names of all registered providers.
func ListProviders() []string {
	providersMu.RLock()
	defer providersMu.RUnlock()
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// HasProvider returns true if a provider is registered for the given name.
func HasProvider(name string) bool {
	providersMu.RLock()
	defer providersMu.RUnlock()
	_, ok := providers[name]
	return ok
}

// resetProviders clears the registry (for testing only).
func resetProviders() {
	providersMu.Lock()
	defer providersMu.Unlock()
	providers = make(map[string]Reconcilable)
}
