package provenance_test

import (
	"errors"
	"testing"

	"github.com/dayvidpham/bestiary"
	"github.com/dayvidpham/provenance"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// ---------------------------------------------------------------------------
// DefaultModelRegistry — Models()
// ---------------------------------------------------------------------------

func TestDefaultModelRegistry_Models(t *testing.T) {
	reg := provenance.DefaultModelRegistry()
	models := reg.Models()

	if len(models) == 0 {
		t.Fatal("Models() returned empty")
	}

	// Verify known models exist (not index-dependent)
	knownModels := []struct {
		provider provenance.Provider
		name     string
	}{
		{provenance.ProviderAnthropic, "claude-opus-4-6"},
		{provenance.ProviderAnthropic, "claude-sonnet-4-6"},
		{provenance.ProviderAnthropic, "claude-haiku-4-5"},
	}
	for _, want := range knownModels {
		if _, ok := reg.Lookup(want.provider, want.name); !ok {
			t.Errorf("Lookup(%s, %q) should succeed", want.provider, want.name)
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
	if len(anthropic) == 0 {
		t.Error("ModelsByProvider(Anthropic) returned empty")
	}
	found := false
	for _, m := range anthropic {
		if string(m.Name) == "claude-opus-4-6" {
			found = true
			break
		}
	}
	if !found {
		t.Error("ModelsByProvider(Anthropic) missing claude-opus-4-6")
	}

	local := reg.ModelsByProvider(provenance.ProviderLocal)
	if len(local) != 0 {
		t.Errorf("ModelsByProvider(Local) = %d, want 0", len(local))
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
		if m.Provider == provider && string(m.Name) == name {
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
// registry (backed by bestiary) must reject a local model with a nonexistent
// name via the registry's Lookup — returning ErrNotFound — without touching the DB.
func TestDefaultRegistry_LookupRejectsBeforeDB(t *testing.T) {
	tr, err := provenance.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer tr.Close()

	_, err = tr.RegisterMLAgent("ns", provenance.RoleWorker,
		provenance.ProviderLocal, "nonexistent-model")
	if !errors.Is(err, provenance.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
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

// ---------------------------------------------------------------------------
// RegistryFromBestiary — round-trip
// ---------------------------------------------------------------------------

func TestRegistryFromBestiary_RoundTrip(t *testing.T) {
	reg := provenance.RegistryFromBestiary(bestiary.Models())
	models := reg.Models()
	if len(models) == 0 {
		t.Fatal("RegistryFromBestiary returned empty registry")
	}

	// Verify a known Anthropic model round-trips
	entry, ok := reg.Lookup(provenance.ProviderAnthropic, "claude-opus-4-6")
	if !ok {
		t.Fatal("Lookup(Anthropic, claude-opus-4-6) failed after round-trip")
	}
	if entry.Provider != provenance.ProviderAnthropic {
		t.Errorf("Provider = %q, want %q", entry.Provider, provenance.ProviderAnthropic)
	}
	if string(entry.Name) != "claude-opus-4-6" {
		t.Errorf("Name = %q, want %q", entry.Name, "claude-opus-4-6")
	}
}
