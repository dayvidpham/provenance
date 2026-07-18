package sqlite

import (
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"zombiezen.com/go/sqlite/sqlitex"
)

// legacy_seed.go holds the OLD-SCHEMA legacy-database test seeding seam
// (docs/journal-relational-contract.md §13). It exists to let base-layer tests and
// the migration proof corpus construct the one input migration actually consumes in
// production: a pre-journal task row that already sits on disk in the OLD schema — a
// row born before the journal existed, so it carries no last_journal_id watermark and
// no journal history. That row is NEVER created through the public API (a native task
// is born journaled via EffectTaskCreate; the direct-write graph/InsertTask creation
// path was retired for the Session SDK). Modelling a legacy row faithfully requires a
// raw write that bypasses the journal, exactly mirroring a legacy database whose tasks
// predate the reducer. Like the adversarial seams in operations_adversarial.go these
// are narrow, test-only *DB methods, never part of the JournalAPI surface.

// SeedLegacyTaskRow raw-inserts one full pre-journal (OLD-schema) task row so a
// base-layer test can exercise the SQL layer over an on-disk row, or migration can
// anchor it (§13). The row is written directly, bypassing the journal: it carries no
// last_journal_id watermark (a pre-journal row reflects no journal position). This is
// the low-level seam the internal SQL-layer suites use in place of the retired
// production task-creation path (graph.Store.AddVertex / db.InsertTask); the sole
// PRODUCTION tasks-row INSERT is the fold's own watermark-carrying insert
// (foldTaskCreateLocked). Callers that only need a legacy status/timestamp shape use
// SeedLegacyTask; callers that assert specific title/type/priority/phase/etc. supply a
// full ptypes.Task here.
func (db *DB) SeedLegacyTaskRow(task ptypes.Task) error {
	if task.ID.Namespace == "" {
		return fmt.Errorf(
			"provenance: SeedLegacyTaskRow requires a namespaced legacy TaskID — where: legacy-database " +
				"test seeding seam (§13); when: before migration; impact: nothing seeded; fix: supply a " +
				"TaskID with a non-empty namespace")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.insertLegacyTaskRowLocked(task)
}

// SeedLegacyTask raw-inserts one pre-journal (OLD-schema) task row so migration can
// anchor it (§13), using the legacy row's status and honest timestamps and placeholder
// metadata for the columns migration does not read. The row is written directly,
// bypassing the journal: it carries no last_journal_id watermark (NULL — a pre-journal
// row reflects no journal position), no owner_id (the committing/owning actor is
// materialised only when migration installs the baseline started-episode, §8.2), and
// its status_id reflects the legacy status verbatim. This is the corpus analogue of a
// legacy database on disk; MigrateLegacyBaseline is what upgrades such a row into
// anchored journal state.
func (db *DB) SeedLegacyTask(row journal.LegacyTaskRow) error {
	if row.ID.Namespace == "" {
		return fmt.Errorf(
			"provenance: SeedLegacyTask requires a namespaced legacy TaskID — where: legacy-database " +
				"test seeding seam (§13); when: before migration; impact: nothing seeded; fix: supply a " +
				"TaskID with a non-empty namespace")
	}
	task := ptypes.Task{
		ID:        row.ID,
		Title:     "legacy task",
		Status:    ptypes.Status(int(row.Status)),
		Priority:  ptypes.PriorityMedium,
		Type:      ptypes.TaskTypeTask,
		Phase:     ptypes.PhaseUnscoped,
		CreatedAt: row.CreatedAt.UTC(),
		UpdatedAt: row.UpdatedAt.UTC(),
		ClosedAt:  row.ClosedAt,
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.insertLegacyTaskRowLocked(task)
}

// insertLegacyTaskRowLocked is the single raw OLD-schema task-row INSERT both legacy
// seams share (§13). The legacy tasks row predates last_journal_id, so it is written
// without a watermark (NULL); migration ADDs the anchor and populates the watermark.
// Assumes db.mu is held.
func (db *DB) insertLegacyTaskRowLocked(task ptypes.Task) error {
	var ownerVal any
	if task.Owner != nil {
		ownerVal = task.Owner.String()
	}
	var closedAt any
	if task.ClosedAt != nil {
		closedAt = task.ClosedAt.UTC().UnixNano()
	}
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO tasks
			(id, namespace, title, description, status_id, priority_id, type_id,
			 phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason)
		 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14)`,
		&sqlitex.ExecOptions{Args: []any{
			task.ID.String(), task.ID.Namespace, task.Title, task.Description,
			int(task.Status), int(task.Priority), int(task.Type), int(task.Phase),
			ownerVal, task.Notes,
			task.CreatedAt.UTC().UnixNano(), task.UpdatedAt.UTC().UnixNano(),
			closedAt, task.CloseReason,
		}}); err != nil {
		return fmt.Errorf("provenance: SeedLegacyTaskRow insert legacy row %q: %w", task.ID, err)
	}
	return nil
}
