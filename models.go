package provenance

import "github.com/dayvidpham/provenance/pkg/ptypes"

// defaultRegistry is the built-in model registry with models matching
// the Anthropic API model identifiers. This is the fallback when no
// external registry (e.g., bestiary) is provided.
type defaultRegistry struct {
	entries []ModelEntry
	index   map[registryKey]ModelEntry
}

type registryKey struct {
	provider Provider
	name     string
}

// defaultModels lists the models seeded into ml_models at database creation.
// Names match the canonical identifiers from models.dev / Anthropic API.
var defaultModels = []ModelEntry{
	{Provider: ProviderAnthropic, Name: "claude-opus-4-6", DisplayName: "Claude Opus 4.6", Family: "claude-opus"},
	{Provider: ProviderAnthropic, Name: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6", Family: "claude-sonnet"},
	{Provider: ProviderAnthropic, Name: "claude-haiku-4-5", DisplayName: "Claude Haiku 4.5", Family: "claude-haiku"},
}

// DefaultModelRegistry returns the built-in model registry.
// It contains the core Anthropic models used by aura agents.
func DefaultModelRegistry() ptypes.ModelRegistry {
	idx := make(map[registryKey]ModelEntry, len(defaultModels))
	for _, m := range defaultModels {
		idx[registryKey{provider: m.Provider, name: m.Name}] = m
	}
	return &defaultRegistry{entries: defaultModels, index: idx}
}

func (r *defaultRegistry) Models() []ModelEntry {
	out := make([]ModelEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

func (r *defaultRegistry) Lookup(provider Provider, name string) (ModelEntry, bool) {
	e, ok := r.index[registryKey{provider: provider, name: name}]
	return e, ok
}
