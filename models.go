package provenance

import (
	"github.com/dayvidpham/bestiary"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// inMemoryRegistry is an in-process model registry backed by a slice of ModelEntry.
// It is the implementation returned by NewRegistry and DefaultModelRegistry.
type inMemoryRegistry struct {
	entries []ModelEntry
	index   map[registryKey]ModelEntry
}

type registryKey struct {
	provider Provider
	name     string
}

// NewRegistry creates a ModelRegistry from the given entries.
// Use this for custom or test registries.
func NewRegistry(entries []ModelEntry) ptypes.ModelRegistry {
	idx := make(map[registryKey]ModelEntry, len(entries))
	for _, m := range entries {
		idx[registryKey{provider: m.Provider, name: string(m.Name)}] = m
	}
	return &inMemoryRegistry{entries: entries, index: idx}
}

// DefaultModelRegistry returns the model registry backed by bestiary.
// It uses bestiary.Models() as the single source of truth.
func DefaultModelRegistry() ptypes.ModelRegistry {
	return RegistryFromBestiary(bestiary.Models())
}

func (r *inMemoryRegistry) Models() []ModelEntry {
	out := make([]ModelEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

func (r *inMemoryRegistry) Lookup(provider Provider, name string) (ModelEntry, bool) {
	e, ok := r.index[registryKey{provider: provider, name: name}]
	return e, ok
}

func (r *inMemoryRegistry) ModelsByProvider(provider Provider) []ModelEntry {
	var out []ModelEntry
	for _, m := range r.entries {
		if m.Provider == provider {
			out = append(out, m)
		}
	}
	return out
}
