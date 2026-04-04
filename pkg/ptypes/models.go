package ptypes

// ModelEntry describes a model known to the registry.
// The (Provider, Name) pair is the unique key used for ml_models seeding.
type ModelEntry struct {
	Provider    Provider // maps to provider_id in ml_models
	Name        string   // model identifier, e.g. "claude-opus-4-6"
	DisplayName string   // human-readable, e.g. "Claude Opus 4.6"
	Family      string   // model family, e.g. "claude-opus"
}

// ModelRegistry provides the set of known ML models for seeding
// the ml_models reference table.
//
// Implementations:
//   - provenance.DefaultModelRegistry() — built-in static models
//   - bestiary.Registry() — full models.dev catalog (future)
type ModelRegistry interface {
	// Models returns all known model entries.
	Models() []ModelEntry

	// Lookup returns the model entry matching the given provider and name,
	// or false if the model is not in the registry.
	Lookup(provider Provider, name string) (ModelEntry, bool)
}
