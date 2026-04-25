package provenance

import (
	"github.com/dayvidpham/bestiary"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// IsKnown reports whether p is a member of the bestiary provider catalog.
//
// This is the catalog-membership check that pkg/ptypes.Provider.IsValid() cannot
// provide (pkg/ptypes is zero-dep and has no access to the bestiary catalog).
// IsValid() only rejects the empty string; IsKnown() verifies the provider
// string is recognized by the bestiary API.
//
// Example:
//
//	provenance.IsKnown(provenance.ProviderAnthropic) // true
//	provenance.IsKnown("completely-unknown-vendor")  // false
func IsKnown(p ptypes.Provider) bool {
	return bestiary.Provider(p).IsKnown()
}

// RegistryFromBestiary converts bestiary model data into a provenance ModelRegistry.
// Only Provider, Name (as ModelID), DisplayName, and Family are extracted.
func RegistryFromBestiary(models []bestiary.ModelInfo) ptypes.ModelRegistry {
	entries := make([]ptypes.ModelEntry, len(models))
	for i, m := range models {
		entries[i] = ptypes.ModelEntry{
			Provider:    ptypes.Provider(m.Provider),
			Name:        ptypes.ModelID(m.ID),
			DisplayName: m.DisplayName,
			Family:      m.Family,
		}
	}
	return NewRegistry(entries)
}
