package provenance_test

import (
	"errors"
	"testing"

	"github.com/dayvidpham/provenance"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// ---------------------------------------------------------------------------
// DefaultModelRegistry — Models()
// ---------------------------------------------------------------------------

func TestDefaultModelRegistry_Models(t *testing.T) {
	reg := provenance.DefaultModelRegistry()
	models := reg.Models()

	if len(models) != 3 {
		t.Fatalf("Models() returned %d entries, want 3", len(models))
	}

	want := []struct {
		provider    provenance.Provider
		name        string
		displayName string
		family      string
	}{
		{provenance.ProviderAnthropic, "claude-opus-4-6", "Claude Opus 4.6", "claude-opus"},
		{provenance.ProviderAnthropic, "claude-sonnet-4-6", "Claude Sonnet 4.6", "claude-sonnet"},
		{provenance.ProviderAnthropic, "claude-haiku-4-5", "Claude Haiku 4.5", "claude-haiku"},
	}

	for i, w := range want {
		got := models[i]
		if got.Provider != w.provider {
			t.Errorf("models[%d].Provider = %v, want %v", i, got.Provider, w.provider)
		}
		if got.Name != w.name {
			t.Errorf("models[%d].Name = %q, want %q", i, got.Name, w.name)
		}
		if got.DisplayName != w.displayName {
			t.Errorf("models[%d].DisplayName = %q, want %q", i, got.DisplayName, w.displayName)
		}
		if got.Family != w.family {
			t.Errorf("models[%d].Family = %q, want %q", i, got.Family, w.family)
		}
	}
}

func TestDefaultModelRegistry_ModelsReturnsCopy(t *testing.T) {
	reg := provenance.DefaultModelRegistry()
	a := reg.Models()
	b := reg.Models()

	// Mutating the returned slice must not affect subsequent calls.
	a[0].Name = "mutated"
	if b[0].Name == "mutated" {
		t.Error("Models() returned a shared slice — must return a copy")
	}
}

// ---------------------------------------------------------------------------
// DefaultModelRegistry — Lookup()
// ---------------------------------------------------------------------------

func TestDefaultModelRegistry_Lookup(t *testing.T) {
	reg := provenance.DefaultModelRegistry()

	// Known model
	entry, ok := reg.Lookup(provenance.ProviderAnthropic, "claude-opus-4-6")
	if !ok {
		t.Fatal("Lookup(ProviderAnthropic, claude-opus-4-6) returned false")
	}
	if entry.DisplayName != "Claude Opus 4.6" {
		t.Errorf("DisplayName = %q, want %q", entry.DisplayName, "Claude Opus 4.6")
	}

	// Unknown model
	_, ok = reg.Lookup(provenance.ProviderAnthropic, "nonexistent")
	if ok {
		t.Error("Lookup(ProviderAnthropic, nonexistent) should return false")
	}

	// Wrong provider
	_, ok = reg.Lookup(provenance.ProviderGoogle, "claude-opus-4-6")
	if ok {
		t.Error("Lookup(ProviderGoogle, claude-opus-4-6) should return false")
	}
}

// ---------------------------------------------------------------------------
// DefaultModelRegistry — ModelsByProvider()
// ---------------------------------------------------------------------------

func TestDefaultModelRegistry_ModelsByProvider(t *testing.T) {
	reg := provenance.DefaultModelRegistry()

	anthropic := reg.ModelsByProvider(provenance.ProviderAnthropic)
	if len(anthropic) != 3 {
		t.Errorf("ModelsByProvider(Anthropic) = %d entries, want 3", len(anthropic))
	}

	google := reg.ModelsByProvider(provenance.ProviderGoogle)
	if len(google) != 0 {
		t.Errorf("ModelsByProvider(Google) = %d entries, want 0", len(google))
	}
}

// ---------------------------------------------------------------------------
// WithModelRegistry — custom registry
// ---------------------------------------------------------------------------

// testRegistry is a minimal ModelRegistry for testing injection.
type testRegistry struct {
	entries []ptypes.ModelEntry
}

func (r *testRegistry) Models() []ptypes.ModelEntry {
	out := make([]ptypes.ModelEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

func (r *testRegistry) Lookup(provider provenance.Provider, name string) (ptypes.ModelEntry, bool) {
	for _, m := range r.entries {
		if m.Provider == provider && m.Name == name {
			return m, true
		}
	}
	return ptypes.ModelEntry{}, false
}

func (r *testRegistry) ModelsByProvider(provider provenance.Provider) []ptypes.ModelEntry {
	var out []ptypes.ModelEntry
	for _, m := range r.entries {
		if m.Provider == provider {
			out = append(out, m)
		}
	}
	return out
}

func TestWithModelRegistry_CustomRegistry(t *testing.T) {
	custom := &testRegistry{
		entries: []ptypes.ModelEntry{
			{Provider: provenance.ProviderGoogle, Name: "gemini-2.0-flash", DisplayName: "Gemini 2.0 Flash", Family: "gemini-flash"},
		},
	}

	tr, err := provenance.OpenMemory(provenance.WithModelRegistry(custom))
	if err != nil {
		t.Fatalf("OpenMemory with custom registry: %v", err)
	}
	defer tr.Close()

	// Custom model should be seeded and usable.
	agent, err := tr.RegisterMLAgent("ns", provenance.RoleWorker, provenance.ProviderGoogle, "gemini-2.0-flash")
	if err != nil {
		t.Fatalf("RegisterMLAgent with custom model failed: %v", err)
	}
	if agent.Model.Name != "gemini-2.0-flash" {
		t.Errorf("Model.Name = %q, want %q", agent.Model.Name, "gemini-2.0-flash")
	}

	// Default models should NOT be seeded.
	_, err = tr.RegisterMLAgent("ns", provenance.RoleWorker, provenance.ProviderAnthropic, "claude-opus-4-6")
	if !errors.Is(err, provenance.ErrNotFound) {
		t.Errorf("RegisterMLAgent with default model: got %v, want errors.Is(err, ErrNotFound)", err)
	}
}

func TestWithModelRegistry_EmptyRegistry(t *testing.T) {
	empty := &testRegistry{entries: nil}

	tr, err := provenance.OpenMemory(provenance.WithModelRegistry(empty))
	if err != nil {
		t.Fatalf("OpenMemory with empty registry: %v", err)
	}
	defer tr.Close()

	// No models seeded — any RegisterMLAgent should fail with ErrNotFound.
	_, err = tr.RegisterMLAgent("ns", provenance.RoleWorker, provenance.ProviderAnthropic, "claude-opus-4-6")
	if !errors.Is(err, provenance.ErrNotFound) {
		t.Errorf("RegisterMLAgent with empty registry: got %v, want errors.Is(err, ErrNotFound)", err)
	}
}

// TestDefaultRegistry_LookupRejectsBeforeDB verifies that the registry Lookup
// fires before any DB layer interaction. A tracker opened with the default
// registry (which seeds 3 Anthropic models) must reject a Google model via
// the registry's Lookup — returning ErrNotFound — without touching the DB.
func TestDefaultRegistry_LookupRejectsBeforeDB(t *testing.T) {
	tr, err := provenance.OpenMemory() // default registry: 3 Anthropic models seeded
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer tr.Close()

	// ProviderGoogle + "gemini-2.0-flash" is not in the default registry.
	// The registry Lookup should reject it before any DB insert is attempted.
	_, err = tr.RegisterMLAgent("ns", provenance.RoleWorker, provenance.ProviderGoogle, "gemini-2.0-flash")
	if !errors.Is(err, provenance.ErrNotFound) {
		t.Errorf("RegisterMLAgent(Google, gemini-2.0-flash) with default registry: got %v, want errors.Is(err, ErrNotFound)", err)
	}
}

func TestWithModelRegistry_NilRegistry(t *testing.T) {
	// Passing untyped nil must not panic — the default registry is preserved.
	tr, err := provenance.OpenMemory(provenance.WithModelRegistry(nil))
	if err != nil {
		t.Fatalf("OpenMemory with nil registry: %v", err)
	}
	defer tr.Close()

	// Default models should still be seeded.
	agent, err := tr.RegisterMLAgent("ns", provenance.RoleWorker, provenance.ProviderAnthropic, "claude-opus-4-6")
	if err != nil {
		t.Fatalf("RegisterMLAgent with nil registry (should use default): %v", err)
	}
	if agent.Model.Name != "claude-opus-4-6" {
		t.Errorf("Model.Name = %q, want %q", agent.Model.Name, "claude-opus-4-6")
	}
}
