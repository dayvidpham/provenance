package sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// seededModelRows returns every (provider, model) pair on the database, read
// back through the provider join so a row with a broken provider surrogate key
// cannot masquerade as a good one.
func seededModelRows(t *testing.T, db *DB) map[string]struct{} {
	t.Helper()
	scope := takePoolScope(t, db)
	defer scope.release()
	rows := map[string]struct{}{}
	if err := scope.queryRows(
		"SELECT providers.name, ml_models.name FROM ml_models JOIN providers ON providers.id = ml_models.provider_id",
		nil,
		func(rows2 *sql.Rows) error {
			var provider, model string
			if err := rows2.Scan(&provider, &model); err != nil {
				return err
			}
			rows[provider+"/"+model] = struct{}{}
			return nil
		},
	); err != nil {
		t.Fatalf("read seeded model rows: %v", err)
	}
	return rows
}

// TestModelSeedWritesEveryKnownModelOnceAndIgnoresUnknownProviders pins the
// contract the prepared model-insert must keep. The statement is prepared once
// and reused across the whole registry, so the properties that used to follow
// from executing the text per row are asserted here instead: every model with a
// seeded provider is written exactly once, a model naming a provider that is not
// in the closed provider lookup is skipped rather than failing the open, and a
// second activation over the same registry adds nothing.
func TestModelSeedWritesEveryKnownModelOnceAndIgnoresUnknownProviders(t *testing.T) {
	t.Parallel()
	models := []ptypes.ModelEntry{
		{Provider: ptypes.ProviderAnthropic, Name: ptypes.ModelID("model-a")},
		{Provider: ptypes.ProviderAnthropic, Name: ptypes.ModelID("model-b")},
		{Provider: ptypes.ProviderOpenAI, Name: ptypes.ModelID("model-a")},
		{Provider: ptypes.ProviderGoogle, Name: ptypes.ModelID("model-c")},
		// The provider lookup is closed and seeded from the enum, so this entry
		// has no provider row to resolve. INSERT OR IGNORE must drop it and leave
		// the open succeeding, exactly as the per-row execution did.
		{Provider: ptypes.Provider("provider-that-is-not-seeded"), Name: ptypes.ModelID("model-d")},
	}
	want := map[string]struct{}{
		"anthropic/model-a": {}, "anthropic/model-b": {},
		"openai/model-a": {}, "google/model-c": {},
	}

	// A private file so the same database can be activated a second time, which
	// is what proves the seed is idempotent rather than merely correct once.
	path := filepath.Join(t.TempDir(), "models.db")
	first, err := Open(path, models)
	if err != nil {
		t.Fatalf("open with model registry: %v", err)
	}
	got := seededModelRows(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("close first handle: %v", err)
	}
	assertModelRows(t, got, want, "first activation")

	second, err := Open(path, models)
	if err != nil {
		t.Fatalf("reopen with the same model registry: %v", err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Errorf("close second handle: %v", err)
		}
	}()
	assertModelRows(t, seededModelRows(t, second), want, "second activation")
}

func assertModelRows(t *testing.T, got, want map[string]struct{}, stage string) {
	t.Helper()
	for key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("%s: model %q was not seeded", stage, key)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("%s: unexpected seeded model %q", stage, key)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s: seeded %d model rows, want %d (%v)", stage, len(got), len(want), fmt.Sprint(got))
	}
}
