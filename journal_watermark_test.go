package provenance

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestMigrationColumnAddPathForColumnlessLegacyDB drives MigrateLegacyBaseline's
// column-add path (§13) against a database whose tasks table predates the
// last_journal_id column entirely. Migration must ADD the column before anchoring, then
// anchoring populates the migrated row's watermark, so a from-empty replay converges,
// the migrated task carries a non-zero watermark, and VerifyIntegrity accepts the
// database. This is the one path where the migration column-add actually fires — the
// shared corpus env models legacy databases via the nullable relax seam, which keeps
// the column present, so this test builds a minimal column-less database directly.
func TestMigrationColumnAddPathForColumnlessLegacyDB(t *testing.T) {
	tr, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	st, ok := tr.(*sqliteTracker)
	if !ok {
		t.Fatal("tracker is not *sqliteTracker")
	}

	sysAgent, err := tr.RegisterSoftwareAgent("provenance-test", "pasture-system", "0", "test")
	if err != nil {
		t.Fatalf("register system actor: %v", err)
	}
	ownerAgent, err := tr.RegisterSoftwareAgent("provenance-test", "actor-frank", "0", "test")
	if err != nil {
		t.Fatalf("register owner actor: %v", err)
	}

	// Genesis bootstrap authority the per-task anchors execute under.
	res, err := tr.Journal().Apply(OperationInput{
		OperationID:    "op-genesis",
		ActorID:        sysAgent.ID,
		CommandDigest:  []byte("genesis-c"),
		MutationDigest: []byte("genesis-m"),
		Effects:        []Effect{{Sort: EffectBootstrapAuthority, BootstrapLabel: "pasture-system", ResultSlot: "auth"}},
	})
	if err != nil {
		t.Fatalf("genesis: %v", err)
	}
	var boot JournalID
	for i := range res.ResultSlots {
		if string(res.ResultSlots[i].Slot) == "auth" {
			boot = res.ResultSlots[i].ProducedJournalID
		}
	}
	if boot == 0 {
		t.Fatal("genesis produced no bootstrap authority slot")
	}

	// Model a legacy database whose tasks table predates last_journal_id entirely.
	if err := st.db.DowngradeTasksToColumnlessLegacy(); err != nil {
		t.Fatalf("downgrade to column-less legacy: %v", err)
	}
	migrated := TaskID{Namespace: "provenance-test", UUID: uuid.Must(uuid.NewV7())}
	base := time.Date(2025, 6, 10, 0, 0, 0, 0, time.UTC)
	if err := st.db.SeedLegacyTask(LegacyTaskRow{
		ID: migrated, RawOwner: "actor-frank", Status: TaskStatusOpen, CreatedAt: base, UpdatedAt: base,
	}); err != nil {
		t.Fatalf("seed column-less legacy task: %v", err)
	}

	if _, err := tr.Journal().MigrateLegacyBaseline(MigrationInput{
		System: sysAgent.ID, BootstrapAuthority: boot,
		Owners: map[string]ActorID{"actor-frank": ownerAgent.ID},
		Legacy: []LegacyTaskRow{{ID: migrated, RawOwner: "actor-frank", Status: TaskStatusOpen, CreatedAt: base, UpdatedAt: base}},
	}); err != nil {
		t.Fatalf("migrate column-less legacy database: %v", err)
	}

	// The from-empty replay reading tasks.last_journal_id both proves the column now
	// exists and converges; the migrated task carries a non-zero anchored watermark.
	replay, err := tr.Journal().ReplayProjections()
	if err != nil {
		t.Fatalf("replay must converge after the column-add + anchor: %v", err)
	}
	proj, ok := replay.ProjectionForTask(migrated)
	if !ok || proj.LastJournalID == 0 {
		t.Fatalf("migrated task not anchored (found=%v watermark=%d)", ok, proj.LastJournalID)
	}
	// Every task row is now anchored, so VerifyIntegrity's watermark gate accepts it.
	if err := tr.Journal().VerifyIntegrity(); err != nil {
		t.Errorf("VerifyIntegrity after migration column-add: %v", err)
	}
}
