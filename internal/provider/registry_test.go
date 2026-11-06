package provider

import (
	"context"
	"testing"
)

// mockProvider is a minimal Provider implementation for testing.
// Only Name() returns a meaningful value; other methods return zero values.
type mockProvider struct {
	name string
}

func (m *mockProvider) Name() string { return m.name }

func (m *mockProvider) ListAvailable(_ context.Context) ([]GPUOffering, error) {
	return nil, nil
}

func (m *mockProvider) Provision(_ context.Context, _ ProvisionRequest) (*ProvisionResult, error) {
	return nil, nil
}

func (m *mockProvider) GetStatus(_ context.Context, _, _ string) (*InstanceStatus, error) {
	return nil, nil
}

func (m *mockProvider) Terminate(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockProvider) ListProvisioned(_ context.Context) ([]ProvisionedInstance, error) {
	return nil, nil
}

func TestRegistryRegisterAndGet(t *testing.T) {
	reg := NewRegistry()
	mock := &mockProvider{name: "charlie"}

	reg.Register(mock)

	// Get existing provider
	p, ok := reg.Get("charlie")
	if !ok {
		t.Fatal("expected to find registered provider 'charlie'")
	}
	if p.Name() != "charlie" {
		t.Fatalf("expected provider name 'charlie', got %q", p.Name())
	}

	// Get unknown provider
	_, ok = reg.Get("unknown")
	if ok {
		t.Fatal("expected Get('unknown') to return false")
	}
}

func TestRegistryAll(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockProvider{name: "charlie"})
	reg.Register(&mockProvider{name: "alpha"})

	all := reg.All()
	if len(all) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(all))
	}

	// Verify both are present (order not guaranteed)
	names := make(map[string]bool)
	for _, p := range all {
		names[p.Name()] = true
	}
	if !names["charlie"] {
		t.Error("expected 'charlie' in All() result")
	}
	if !names["alpha"] {
		t.Error("expected 'alpha' in All() result")
	}
}

func TestRegistryNames(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockProvider{name: "charlie"})
	reg.Register(&mockProvider{name: "alpha"})
	reg.Register(&mockProvider{name: "bravo"})

	names := reg.Names()
	if len(names) != 3 {
		t.Fatalf("expected 3 names, got %d", len(names))
	}

	// Names() returns sorted list
	expected := []string{"alpha", "bravo", "charlie"}
	for i, name := range names {
		if name != expected[i] {
			t.Errorf("expected names[%d] = %q, got %q", i, expected[i], name)
		}
	}
}

func TestRegistryReRegister(t *testing.T) {
	reg := NewRegistry()

	original := &mockProvider{name: "charlie"}
	replacement := &mockProvider{name: "charlie"}

	reg.Register(original)
	reg.Register(replacement)

	p, ok := reg.Get("charlie")
	if !ok {
		t.Fatal("expected to find provider 'charlie' after re-registration")
	}

	// Verify we got the replacement, not the original
	if p != replacement {
		t.Fatal("expected Get to return the replacement provider after re-registration")
	}

	// Should still only have one provider
	all := reg.All()
	if len(all) != 1 {
		t.Fatalf("expected 1 provider after re-registration, got %d", len(all))
	}
}
