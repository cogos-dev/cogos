// reconcile_registry_test.go
// Tests for the provider registry.

package main

import (
	"context"
	"testing"
)

// mockProvider implements Reconcilable for testing.
type mockProvider struct {
	name string
}

func (m *mockProvider) Type() string                     { return m.name }
func (m *mockProvider) LoadConfig(string) (any, error)   { return nil, nil }
func (m *mockProvider) FetchLive(_ context.Context, _ any) (any, error) {
	return nil, nil
}
func (m *mockProvider) ComputePlan(_ any, _ any, _ *ReconcileState) (*ReconcilePlan, error) {
	return nil, nil
}
func (m *mockProvider) ApplyPlan(_ context.Context, _ *ReconcilePlan) ([]ReconcileResult, error) {
	return nil, nil
}
func (m *mockProvider) BuildState(_ any, _ any, _ *ReconcileState) (*ReconcileState, error) {
	return nil, nil
}
func (m *mockProvider) Health() ResourceStatus {
	return NewResourceStatus(SyncStatusUnknown, HealthMissing)
}

func TestRegisterAndGetProvider(t *testing.T) {
	resetProviders()
	defer resetProviders()

	RegisterProvider("test", &mockProvider{name: "test"})

	p, err := GetProvider("test")
	if err != nil {
		t.Fatalf("GetProvider failed: %v", err)
	}
	if p.Type() != "test" {
		t.Errorf("Type() = %q, want %q", p.Type(), "test")
	}
}

func TestGetProviderUnknown(t *testing.T) {
	resetProviders()
	defer resetProviders()

	_, err := GetProvider("nonexistent")
	if err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestListProviders(t *testing.T) {
	resetProviders()
	defer resetProviders()

	RegisterProvider("discord", &mockProvider{name: "discord"})
	RegisterProvider("agent", &mockProvider{name: "agent"})

	list := ListProviders()
	if len(list) != 2 {
		t.Fatalf("ListProviders() = %v, want 2 entries", list)
	}
	// Should be sorted
	if list[0] != "agent" || list[1] != "discord" {
		t.Errorf("ListProviders() = %v, want [agent, discord]", list)
	}
}

func TestHasProvider(t *testing.T) {
	resetProviders()
	defer resetProviders()

	RegisterProvider("discord", &mockProvider{name: "discord"})

	if !HasProvider("discord") {
		t.Error("HasProvider(discord) = false, want true")
	}
	if HasProvider("agent") {
		t.Error("HasProvider(agent) = true, want false")
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	resetProviders()
	defer resetProviders()

	RegisterProvider("test", &mockProvider{name: "test"})

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for duplicate registration")
		}
	}()
	RegisterProvider("test", &mockProvider{name: "test"})
}

func TestListProvidersEmpty(t *testing.T) {
	resetProviders()
	defer resetProviders()

	list := ListProviders()
	if len(list) != 0 {
		t.Errorf("ListProviders() = %v, want empty", list)
	}
}
