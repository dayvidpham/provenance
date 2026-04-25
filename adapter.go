package provenance

import (
	"strings"

	"github.com/dayvidpham/bestiary"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// IsKnown reports whether p is a member of the bestiary provider catalog.
//
// IsKnown verifies the provider string is recognized by the bestiary API.
//
// Example:
//
//	provenance.IsKnown(provenance.ProviderAnthropic) // true
//	provenance.IsKnown("completely-unknown-vendor")  // false
func IsKnown(p ptypes.Provider) bool {
	return bestiary.Provider(p).IsKnown()
}

// IsValid reports whether p is a valid, catalog-known provider.
//
// URD R9: validation must delegate to the bestiary catalog (bestiary.Provider.IsKnown),
// not merely check for a non-empty string. This function lives in the root provenance
// package (not pkg/ptypes) because pkg/ptypes is a leaf package with no bestiary import.
//
// Semantics:
//   - Empty or whitespace-only strings return false.
//   - Catalog membership is case-sensitive: "anthropic" → true, "ANTHROPIC" → false.
//   - Any non-empty string not in the bestiary catalog returns false.
//
// Example:
//
//	provenance.IsValid(provenance.ProviderAnthropic)         // true
//	provenance.IsValid(ptypes.Provider("ANTHROPIC"))         // false (case-sensitive)
//	provenance.IsValid(ptypes.Provider(""))                  // false
//	provenance.IsValid(ptypes.Provider("nonexistent-vendor")) // false (not in catalog)
func IsValid(p ptypes.Provider) bool {
	if strings.TrimSpace(string(p)) == "" {
		return false
	}
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
			Family:      string(m.Family),
		}
	}
	return NewRegistry(entries)
}
