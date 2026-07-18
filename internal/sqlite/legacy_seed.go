package sqlite

import (
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	"zombiezen.com/go/sqlite/sqlitex"
)

// legacy_seed.go holds the OLD-SCHEMA legacy-database test seeding seam
// (docs/journal-relational-contract.md §13). It exists to let the migration proof
// corpus construct the one input migration actually consumes in production: a
// pre-journal task row that already sits on disk in the OLD schema — a row born
// before the journal existed, so it carries no last_journal_id watermark and no
// journal history. That row is NEVER created through the public API (a native task
// is born journaled via EffectTaskCreate); modelling it faithfully requires a raw
// write that bypasses the journal, exactly mirroring a legacy database whose tasks
// predate the reducer. Like the adversarial seams in operations_adversarial.go this
// is a narrow, test-only *DB method, never part of the JournalAPI surface.

// SeedLegacyTask raw-inserts one pre-journal (OLD-schema) task row so migration can
// anchor it (§13). The row is written directly, bypassing the journal: it carries no
// last_journal_id watermark (NULL — a pre-journal row reflects no journal position),
// no owner_id (the committing/owning actor is materialised only when migration
// installs the baseline started-episode, §8.2), and its status_id reflects the
// legacy status verbatim. This is the corpus analogue of a legacy database on disk;
// MigrateLegacyBaseline is what upgrades such a row into anchored journal state.
func (db *DB) SeedLegacyTask(row journal.LegacyTaskRow) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if row.ID.Namespace == "" {
		return fmt.Errorf(
			"provenance: SeedLegacyTask requires a namespaced legacy TaskID — where: legacy-database " +
				"test seeding seam (§13); when: before migration; impact: nothing seeded; fix: supply a " +
				"TaskID with a non-empty namespace")
	}
	createdAt := row.CreatedAt.UTC().UnixNano()
	updatedAt := row.UpdatedAt.UTC().UnixNano()
	var closedAt any
	if row.ClosedAt != nil {
		closedAt = row.ClosedAt.UTC().UnixNano()
	}
	// The legacy tasks row predates last_journal_id, so it is written without a
	// watermark (NULL). Migration ADDs the anchor and populates the watermark.
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO tasks
			(id, namespace, title, description, status_id, priority_id, type_id,
			 phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason)
		 VALUES (?1, ?2, 'legacy task', '', ?3, 2, 2, 12, NULL, '', ?4, ?5, ?6, '')`,
		&sqlitex.ExecOptions{Args: []any{
			row.ID.String(), row.ID.Namespace, int(row.Status),
			createdAt, updatedAt, closedAt,
		}}); err != nil {
		return fmt.Errorf("provenance: SeedLegacyTask insert legacy row %q: %w", row.ID, err)
	}
	return nil
}
