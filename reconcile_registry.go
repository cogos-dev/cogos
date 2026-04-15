// reconcile_registry.go
// Thin re-export layer: provider registry delegates to pkg/reconcile.

package main

import "github.com/cogos-dev/cogos/pkg/reconcile"

// RegisterProvider adds a reconciliation provider to the global registry.
func RegisterProvider(name string, provider Reconcilable) {
	reconcile.RegisterProvider(name, provider)
}

// GetProvider returns the provider for the given resource type.
func GetProvider(name string) (Reconcilable, error) {
	return reconcile.GetProvider(name)
}

// ListProviders returns sorted names of all registered providers.
func ListProviders() []string {
	return reconcile.ListProviders()
}

// HasProvider returns true if a provider is registered for the given name.
func HasProvider(name string) bool {
	return reconcile.HasProvider(name)
}

// resetProviders clears the registry (for testing only).
func resetProviders() {
	reconcile.ResetProviders()
}
