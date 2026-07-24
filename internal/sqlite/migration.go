package sqlite

import (
	"context"
	"fmt"
	"sort"

	"github.com/dayvidpham/provenance/internal/journal"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// migration.go implements legacy-baseline migration and the external-schema
// preflight (docs/journal-relational-contract.md §13). Migration installs one
// deterministic baseline anchor per legacy task under the genesis bootstrap
// authority, in a deterministic (created_at, id) pre-migration order, with honest
// legacy RecordedAt provenance and whole-batch fail-closed atomicity. Preflight
// verifies the pre-journal schema's exact expected shape in both directions before
// any transaction opens, failing closed with a typed SchemaPreflightError.

// ---------------------------------------------------------------------------
// External-schema preflight (§13)
// ---------------------------------------------------------------------------

// expectedTable is one journal relation whose exact shape preflight enforces.
type expectedTable struct {
	name    string
	columns []string
}

// expectedJournalShape is the exact table/column shape this build understands
// (§13 preflight). Preflight checks presence in both directions: a missing table,
// a missing expected column, or an unexpected extra column all fail closed. The
// set is the core journal spine; the ordinary reference/task tables are not
// enumerated here because migration only writes journal rows.
var expectedJournalShape = []expectedTable{
	{"journal", []string{"journal_id", "kind_id", "actor_id", "recorded_at", "produced_by_operation_journal_id"}},
	{"journal_task_events", []string{"journal_id", "task_id", "event_kind", "payload"}},
	{"journal_operations", []string{"journal_id", "operation_id", "authority_journal_id", "command_digest", "mutation_digest", "mutation_encoding_version", "canonical_mutation"}},
	{"journal_authorities", []string{"journal_id", "authority_kind_id", "operation_authority_id"}},
}

// recognizedJournalSpineTables is the CLOSED set of journal-spine relations
// (every `journal`-prefixed table this build creates, §3-§6). Preflight's
// extra-table direction (§13: "an unexpected extra table") enumerates the live
// `journal`-prefixed tables and fails closed on any name outside this set — a
// stray migration artifact, a manually-created table, or a future schema
// version's table left behind after a downgrade — so a partially-provisioned
// deployment halts rather than proceeding against a shape it does not fully
// understand. Non-journal-spine relations (tasks/agents/edges/…) are not
// enumerated here: they are matched by the `journal`-prefix convention below, so
// they are never mistaken for a stray spine table.
var recognizedJournalSpineTables = map[string]struct{}{
	"journal":                                  {},
	"journal_kinds":                            {},
	"journal_task_events":                      {},
	"journal_task_event_contexts":              {},
	"journal_operations":                       {},
	"journal_operation_result_slots":           {},
	"journal_authorities":                      {},
	"journal_authority_bootstraps":             {},
	"journal_authority_assignment_episodes":    {},
	"journal_authority_assignment_transitions": {},
	"journal_decisions":                        {},
	"journal_evidence":                         {},
	"journal_activity_creations":               {},
}

// PreflightSchema verifies the external pre-journal schema shape (§13). It is a
// read-only check that runs in its own snapshot before any write transaction opens; any mismatch is
// a typed SchemaPreflightError and no row of any kind is written.
func (db *DB) PreflightSchema() (err error) {
	scope, err := db.bindJournalScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("PreflightSchema: lease connection: %w", err)
	}
	defer scope.release()
	endTx := sqlitex.Save(scope.conn)
	defer endTx(&err)
	return scope.preflightSchemaLocked()
}

func (db *connScope) preflightSchemaLocked() error {
	for _, want := range expectedJournalShape {
		present, err := db.tableExistsLocked(want.name)
		if err != nil {
			return err
		}
		if !present {
			return &journal.SchemaPreflightError{
				Operation:     "PreflightSchema",
				ExpectedShape: "table " + want.name,
				FoundShape:    "table " + want.name + " absent",
				Stage:         "table-presence check for " + want.name,
				Why:           "the live schema is missing a table this build requires before migration/replay can proceed",
				Impact:        "no row of any kind is written; activation halts before any write transaction opens",
				Fix:           "restore the expected schema shape or run the correct forward migration, then re-open",
			}
		}
		actual, err := db.tableColumnsLocked(want.name)
		if err != nil {
			return err
		}
		if want.name == "journal_operations" && isLegacyOperationsColumnSet(actual) {
			continue
		}
		if perr := checkColumns(want, actual); perr != nil {
			return perr
		}
	}
	// Extra-table direction (§13): every live `journal`-prefixed table must be a
	// recognized journal-spine relation. An unrecognized one fails closed.
	return db.preflightNoUnexpectedSpineTableLocked()
}

func isLegacyOperationsColumnSet(actual map[string]struct{}) bool {
	legacy := []string{"journal_id", "operation_id", "authority_journal_id", "command_digest", "mutation_digest"}
	if len(actual) != len(legacy) {
		return false
	}
	for _, column := range legacy {
		if _, ok := actual[column]; !ok {
			return false
		}
	}
	return true
}

// preflightNoUnexpectedSpineTableLocked enumerates every live table whose name
// follows the journal-spine convention (name = 'journal' or name LIKE 'journal_%')
// and fails closed with a typed SchemaPreflightError on any name outside the
// closed recognizedJournalSpineTables set — the extra-table direction §13's prose
// promises alongside the extra-column direction (§13, checkColumns).
func (db *connScope) preflightNoUnexpectedSpineTableLocked() error {
	var unexpected string
	if err := sqlitex.Execute(db.conn, "SELECT name FROM sqlite_master WHERE type=?1\n\t\t   AND (name = ?2 OR name LIKE ?3 ESCAPE ?4)\n\t\t ORDER BY name ASC", &sqlitex.ExecOptions{Args: []any{"table", "journal", `journal\_%`, `\`}, ResultFunc: func(stmt *zs.Stmt) error {
		name := stmt.ColumnText(0)
		if _, ok := recognizedJournalSpineTables[name]; !ok && unexpected == "" {
			unexpected = name
		}
		return nil
	}}); err != nil {
		return fmt.Errorf("preflight: enumerate journal-spine tables: %w", err)
	}
	if unexpected != "" {
		return &journal.SchemaPreflightError{
			Operation:     "PreflightSchema",
			ExpectedShape: "only recognized journal-spine tables present",
			FoundShape:    "unexpected extra table " + unexpected,
			Stage:         "table-exclusivity check across the journal-spine set",
			Why:           "the live schema carries a journal-spine-named table outside any migration this build recognizes, so its shape is not fully understood",
			Impact:        "no row of any kind is written; activation halts rather than proceeding against an unreviewed schema",
			Fix:           "revert the out-of-band table addition or teach this build the migration that added it, then re-open",
		}
	}
	return nil
}

// checkColumns compares the actual column set against the expected one in both
// directions (§13): a missing expected column or an unexpected extra column each
// fail closed.
func checkColumns(want expectedTable, actual map[string]struct{}) error {
	expected := make(map[string]struct{}, len(want.columns))
	for _, c := range want.columns {
		expected[c] = struct{}{}
		if _, ok := actual[c]; !ok {
			return &journal.SchemaPreflightError{
				Operation:     "PreflightSchema",
				ExpectedShape: fmt.Sprintf("column %s.%s", want.name, c),
				FoundShape:    fmt.Sprintf("column %s.%s absent", want.name, c),
				Stage:         fmt.Sprintf("column-presence check for %s.%s", want.name, c),
				Why:           "the live schema is missing an expected column this build reads",
				Impact:        "no row of any kind is written; activation halts before any write transaction opens",
				Fix:           "restore the expected column or run the correct forward migration, then re-open",
			}
		}
	}
	for c := range actual {
		if _, ok := expected[c]; !ok {
			return &journal.SchemaPreflightError{
				Operation:     "PreflightSchema",
				ExpectedShape: fmt.Sprintf("table %s with exactly its known columns", want.name),
				FoundShape:    fmt.Sprintf("unexpected extra column %s.%s", want.name, c),
				Stage:         fmt.Sprintf("column-exclusivity check for %s", want.name),
				Why:           "the live schema carries a column outside any migration this build recognizes, so its shape is not fully understood",
				Impact:        "no row of any kind is written; activation halts rather than proceeding against an unreviewed schema",
				Fix:           "revert the out-of-band schema change or teach this build the migration that added it, then re-open",
			}
		}
	}
	return nil
}

func (db *connScope) tableExistsLocked(table string) (bool, error) {
	present := false
	if err := sqlitex.Execute(db.conn,
		"SELECT ?3 FROM sqlite_master WHERE type=?1 AND name=?2",
		&sqlitex.ExecOptions{Args: []any{"table", table, 1}, ResultFunc: func(*zs.Stmt) error { present = true; return nil }}); err != nil {
		return false, fmt.Errorf("preflight: probe table %q: %w", table, err)
	}
	return present, nil
}

func (db *connScope) tableColumnsLocked(table string) (map[string]struct{}, error) {
	cols := map[string]struct{}{}
	if err := sqlitex.Execute(db.conn, "SELECT name FROM pragma_table_info(?1)", &sqlitex.ExecOptions{Args: []any{table}, ResultFunc: func(stmt *zs.Stmt) error {
		cols[stmt.ColumnText(0)] = struct{}{}
		return nil
	}}); err != nil {
		return nil, fmt.Errorf("preflight: read columns of %q: %w", table, err)
	}
	return cols, nil
}

// P2 compatibility adapters for db.go and the unchanged Apply slice. The
// callers already own db.conn for their full activation/operation lifetime.
func (db *DB) tableExistsLocked(table string) (bool, error) {
	return borrowConnScope(db.conn, db.projectionTarget).tableExistsLocked(table)
}

func (db *DB) tableColumnsLocked(table string) (map[string]struct{}, error) {
	return borrowConnScope(db.conn, db.projectionTarget).tableColumnsLocked(table)
}

// ---------------------------------------------------------------------------
// Legacy-baseline migration (§13)
// ---------------------------------------------------------------------------

// MigrateLegacyBaseline migrates a set of pre-journal tasks into baseline journal
// entries (§13): one migration-marker event per task, a started owner-responsibility
// transition for a legacy-owned task, and an ended transition for a legacy-terminal
// task — all under the genesis bootstrap authority, in deterministic (created_at,
// id) order, with honest legacy RecordedAt provenance. The whole run is one
// transaction: an unmappable owner or any fault rolls back every baseline for every
// task (§13 item 4, §9.5). The deterministic per-task OperationID makes a re-run
// idempotent (§9.4).
func (db *DB) MigrateLegacyBaseline(in journal.MigrationInput) (journal.MigrationResult, error) {
	scope, err := db.bindJournalScope(context.Background(), projectionTargetLive)
	if err != nil {
		return journal.MigrationResult{}, fmt.Errorf("MigrateLegacyBaseline: lease connection: %w", err)
	}
	defer scope.release()
	return scope.migrateLockedWithFault(in, nil)
}

func (db *connScope) migrateLockedWithFault(in journal.MigrationInput, faultHook func(taskIndex int) error) (journal.MigrationResult, error) {
	// Preflight strictly before any write transaction opens (§13).
	if err := db.preflightSchemaLocked(); err != nil {
		return journal.MigrationResult{}, err
	}

	// Column-add path (§13): a legacy database that predates last_journal_id gets the
	// column added (nullable, with the journal FK) before any row is anchored, so the
	// anchoring projection has a column to write each row's watermark into. Idempotent —
	// a no-op when the column is already present (the pre-tightening nullable shape).
	if err := db.ensureTasksWatermarkColumnLocked(); err != nil {
		return journal.MigrationResult{}, err
	}

	// Deterministic pre-migration order: created_at ascending, then id ascending
	// (id alone breaks every tie, §13). Sort a copy so the caller's slice is
	// untouched and the order never depends on map/iteration nondeterminism.
	ordered := make([]journal.LegacyTaskRow, len(in.Legacy))
	copy(ordered, in.Legacy)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
		}
		return ordered[i].ID.String() < ordered[j].ID.String()
	})

	// Resolve every legacy owner BEFORE opening the write transaction: an
	// unmappable owner fails the whole batch with the typed six-field error and
	// nothing is written (§13 item 4, §13.1).
	resolved := make([]*journal.ActorID, len(ordered))
	for i, lt := range ordered {
		if lt.RawOwner == "" {
			continue
		}
		owner, ok := in.Owners[lt.RawOwner]
		if !ok {
			return journal.MigrationResult{}, &journal.MigrationOwnerUnmappableError{
				Operation: "MigrateLegacyBaseline",
				Task:      lt.ID,
				RawOwner:  lt.RawOwner,
				Stage:     "owner resolution, before any baseline row for the run is committed",
				Why:       "the legacy owner string resolved to no registered ActorID",
				Impact:    "the entire migration transaction is rolled back; zero baselines are committed for any task in the run",
				Fix:       "register the owner as an actor, or correct/remove the legacy owner string, then re-run (the deterministic OperationID scheme makes the re-run idempotent)",
			}
		}
		o := owner
		resolved[i] = &o
	}

	// Anchor every legacy row in one whole-batch transaction (§9.5, §13).
	result, err := db.anchorLegacyBaselinesLocked(in, ordered, resolved, faultHook)
	if err != nil {
		return journal.MigrationResult{}, err
	}

	// Re-tighten the watermark to schema-level NOT NULL (§8.1, §13) once EVERY task row
	// carries a watermark. A full legacy migration anchors the whole tasks table, so this
	// restores the same NOT NULL shape a fresh native database ships: the migrated database
	// converges to structural enforcement, not merely data-level satisfaction, so a future
	// un-journaled NULL-watermark write is rejected by the schema itself (not only detected
	// later by VerifyIntegrity/ReplayProjections). If un-anchored legacy rows remain (the
	// caller migrated only a subset), the table cannot yet satisfy NOT NULL, so the column
	// stays at the legacy nullable shape and a later migration of the rest completes the
	// tightening — the guard below keeps the rebuild fail-closed rather than erroring on a
	// still-NULL row. The canonical SQLite table rebuild toggles PRAGMA foreign_keys, which
	// is a no-op inside a transaction, so the rebuild runs its own FK-safe atomic
	// transaction immediately after the anchor batch commits rather than nesting inside the
	// anchor savepoint — still on the single operation lease, so no other writer observes the
	// intermediate shape. Should the rebuild itself fail, the committed anchors are left in
	// the valid legacy nullable shape; a re-run is idempotent (§9.4) and re-applies it.
	unanchored, err := db.countUnanchoredTasksLocked()
	if err != nil {
		return journal.MigrationResult{}, err
	}
	if unanchored == 0 {
		if err := db.rebuildTasksWatermarkLocked(tasksWatermarkNative); err != nil {
			return journal.MigrationResult{}, fmt.Errorf(
				"MigrateLegacyBaseline: re-tighten tasks.last_journal_id to NOT NULL — where: post-anchor "+
					"schema re-tightening (§8.1, §13); when: after every legacy row was anchored and its "+
					"watermark populated; impact: the baseline anchors are committed but the watermark column "+
					"remains at the legacy nullable shape; fix: re-run MigrateLegacyBaseline — the deterministic "+
					"per-task OperationID makes the anchor phase idempotent and the re-tightening re-applies: %w", err)
		}
	}

	return result, nil
}

// anchorLegacyBaselinesLocked folds one deterministic baseline operation per legacy task
// as a single whole-batch savepoint transaction (§9.5, §13): a fault (an injected
// faultHook or any apply error) rolls back every baseline written so far, committing
// nothing for any task in the run. On success the savepoint commits when this function
// returns, so the caller's subsequent watermark re-tightening rebuild — which needs the
// foreign-keys pragma that is a no-op inside a transaction — runs cleanly against the
// committed, fully-anchored table. Assumes db.conn is operation-owned; ordered/resolved are the
// deterministically-ordered legacy rows and their pre-resolved owners.
func (db *connScope) anchorLegacyBaselinesLocked(in journal.MigrationInput, ordered []journal.LegacyTaskRow, resolved []*journal.ActorID, faultHook func(taskIndex int) error) (journal.MigrationResult, error) {
	var txErr error
	endTx := sqlitex.Save(db.conn) // whole-batch savepoint (§9.5, §13)
	defer endTx(&txErr)

	result := journal.MigrationResult{TasksMigrated: len(ordered)}
	for i, lt := range ordered {
		op := baselineOperation(in, lt, resolved[i])
		if err := validateApplyInput(op); err != nil {
			txErr = err
			return journal.MigrationResult{}, txErr
		}
		// Honest-timestamp guard (regression g): every migration-sourced row's
		// RecordedAt must trace to a legacy column value (§13, §12).
		if err := assertHonestBaselineTimestamps(lt, op); err != nil {
			txErr = err
			return journal.MigrationResult{}, txErr
		}
		res, err := db.boundDB().applyLocked(op, nil)
		if err != nil {
			txErr = err
			return journal.MigrationResult{}, fmt.Errorf("MigrateLegacyBaseline: baseline for task %s: %w", lt.ID.String(), err)
		}
		if res.ShortCircuited {
			result.ShortCircuited++
		} else {
			result.BaselineAnchorsCreated++
		}
		// Whole-batch fail-closed seam (§9.5): a fault after task i rolls back every
		// baseline written so far, including the schema-marker-equivalent anchors.
		if faultHook != nil {
			if err := faultHook(i); err != nil {
				txErr = fmt.Errorf(
					"%w: migration faulted after baseline %d of %d — why: %v; where: MigrateLegacyBaseline "+
						"whole-batch transaction; when: mid-run, before the run completed; impact: every "+
						"baseline anchor, episode, and transition written in this run is rolled back atomically, "+
						"zero committed for any task; fix: resolve the fault condition and re-run (the "+
						"deterministic per-task OperationID makes the re-run idempotent)",
					journal.ErrMigrationFault, i, len(ordered), err)
				return journal.MigrationResult{}, txErr
			}
		}
	}
	return result, nil
}

// baselineOperation builds the deterministic per-task baseline operation (§13):
// a migration-marker event (RecordedAt = legacy updated_at), a started
// owner-responsibility transition for a legacy-owned task (RecordedAt = legacy
// updated_at), and an ended transition for a legacy-terminal task (RecordedAt =
// legacy closed_at, else updated_at) — never the migration's own wall-clock time.
func baselineOperation(in journal.MigrationInput, lt journal.LegacyTaskRow, owner *journal.ActorID) journal.OperationInput {
	auth := in.BootstrapAuthority
	updatedAt := lt.UpdatedAt.UTC().UnixNano()
	op := journal.OperationInput{
		OperationID:        journal.MigrationBaselineOperationID(lt.ID),
		ActorID:            in.System,
		AuthorityJournalID: &auth,
		CommandDigest:      []byte("provenance.migration.command--" + lt.ID.String()),
		MutationDigest:     []byte("provenance.migration.mutation--" + lt.ID.String()),
		RecordedAt:         updatedAt,
	}
	updated := updatedAt
	// 1. Migration-marker event, honest legacy updated_at (§13 item 1). Its payload
	//    captures the legacy status verbatim so the status projection is reproducible
	//    solely from journal history — the shared reducer seeds status from THIS
	//    captured value, never from the mutable tasks row (§13, §15).
	op.Effects = append(op.Effects, journal.Effect{
		Sort:               journal.EffectTaskEvent,
		TaskID:             lt.ID,
		EventKind:          journal.EventKindTaskMigrated,
		Payload:            journal.EncodeMigrationMarkerPayload(lt.Status),
		RecordedAtOverride: &updated,
	})
	if owner == nil {
		return op // fresh (no-owner) baseline: marker only (§13 item 1)
	}
	// 2. Started owner-responsibility transition, honest legacy updated_at (§13 item 2).
	assignment := journal.MigrationBaselineAssignmentID(lt.ID)
	op.Effects = append(op.Effects, journal.Effect{
		Sort:               journal.EffectAssignmentStart,
		AssignmentID:       assignment,
		TaskID:             lt.ID,
		SlotID:             journal.SlotOwnerResponsibility,
		Occupant:           *owner,
		RecordedAtOverride: &updated,
	})
	// 3. Legacy-terminal: ended transition, honest legacy closed_at (falling back
	//    to updated_at), never wall-clock now (§13 item 3).
	if lt.Status == journal.TaskStatusClosed {
		endedAt := updatedAt
		if lt.ClosedAt != nil {
			endedAt = lt.ClosedAt.UTC().UnixNano()
		}
		ended := endedAt
		op.Effects = append(op.Effects, journal.Effect{
			Sort:               journal.EffectAssignmentEnd,
			AssignmentID:       assignment,
			TaskID:             lt.ID,
			SlotID:             journal.SlotOwnerResponsibility,
			RecordedAtOverride: &ended,
		})
	}
	return op
}

// assertHonestBaselineTimestamps rejects a baseline operation any of whose effect
// RecordedAt overrides does not trace to one of the legacy row's own timestamp
// columns (updated_at, or closed_at for a terminal task) — regression (g). A
// wall-clock value taken during migration traces to no legacy column and is
// rejected before any write (§13 items 1-3, §13.1).
func assertHonestBaselineTimestamps(lt journal.LegacyTaskRow, op journal.OperationInput) error {
	legal := map[int64]struct{}{lt.UpdatedAt.UTC().UnixNano(): {}}
	if lt.ClosedAt != nil {
		legal[lt.ClosedAt.UTC().UnixNano()] = struct{}{}
	}
	for i := range op.Effects {
		ts := op.Effects[i].RecordedAtOverride
		if ts == nil {
			continue // inherits the operation RecordedAt (itself the legacy updated_at)
		}
		if _, ok := legal[*ts]; !ok {
			return fmt.Errorf(
				"%w: legacy task %s effect %d carries RecordedAt %d which is neither the legacy "+
					"updated_at nor closed_at — where: migration honest-timestamp guard (§13); when: "+
					"before any baseline row is committed; impact: nothing committed; fix: stamp a "+
					"migration-sourced row only with the legacy row's own updated_at/closed_at, never a "+
					"wall-clock read",
				journal.ErrDishonestMigrationTimestamp, lt.ID.String(), i, *ts)
		}
	}
	return nil
}

// CountBaselineAnchors returns how many committed operations are legacy-baseline
// anchors (their OperationID carries the deterministic migration prefix, §13). It
// is an audit/read helper proving a re-run created no duplicate baseline.
func (db *DB) CountBaselineAnchors() (n int, err error) {
	scope, err := db.bindJournalScope(context.Background(), projectionTargetLive)
	if err != nil {
		return 0, fmt.Errorf("CountBaselineAnchors: lease connection: %w", err)
	}
	defer scope.release()
	endTx := sqlitex.Save(scope.conn)
	defer endTx(&err)
	if err := sqlitex.Execute(scope.conn, "SELECT COUNT(*) FROM journal_operations WHERE operation_id LIKE ?1", &sqlitex.ExecOptions{Args: []any{"provenance.migration.baseline--%"}, ResultFunc: func(stmt *zs.Stmt) error { n = stmt.ColumnInt(0); return nil }}); err != nil {
		return 0, fmt.Errorf("CountBaselineAnchors: %w", err)
	}
	return n, nil
}

// StartedTransitionRecordedAt returns the RecordedAt (UnixNano) stamped on the
// started transition of a migration episode, so a corpus history can assert the
// honest legacy timestamp was used (§13). ok is false when no such episode exists.
func (db *DB) EpisodeTransitionRecordedAt(assignment journal.AssignmentID, started bool) (recordedAt int64, found bool, err error) {
	scope, err := db.bindJournalScope(context.Background(), projectionTargetLive)
	if err != nil {
		return 0, false, fmt.Errorf("EpisodeTransitionRecordedAt: lease connection: %w", err)
	}
	defer scope.release()
	endTx := sqlitex.Save(scope.conn)
	defer endTx(&err)
	transition := transitionEndedID
	if started {
		transition = transitionStartedID
	}
	if err := sqlitex.Execute(scope.conn, "SELECT j.recorded_at FROM journal_authority_assignment_transitions t\n\t\t JOIN journal j ON j.journal_id = t.journal_id\n\t\t WHERE t.assignment_id = ?1 AND t.transition_id = ?2", &sqlitex.ExecOptions{Args: []any{string(assignment), transition}, ResultFunc: func(stmt *zs.Stmt) error {
		found = true
		recordedAt = stmt.ColumnInt64(0)
		return nil
	}}); err != nil {
		return 0, false, fmt.Errorf("EpisodeTransitionRecordedAt %q: %w", assignment, err)
	}
	return recordedAt, found, nil
}

// EpisodeActive reports whether an episode has a started transition and no ended
// transition (§8.4), so a corpus history can assert a migrated episode's activity.
func (db *DB) EpisodeActive(assignment journal.AssignmentID) (active bool, err error) {
	scope, err := db.bindJournalScope(context.Background(), projectionTargetLive)
	if err != nil {
		return false, fmt.Errorf("EpisodeActive: lease connection: %w", err)
	}
	defer scope.release()
	endTx := sqlitex.Save(scope.conn)
	defer endTx(&err)
	started, err := scope.boundDB().transitionExistsLocked(assignment, transitionStartedID)
	if err != nil {
		return false, err
	}
	ended, err := scope.boundDB().transitionExistsLocked(assignment, transitionEndedID)
	if err != nil {
		return false, err
	}
	return started && !ended, nil
}

// CountEpisodesForTask returns how many owner-responsibility episodes exist for a
// task, so a corpus history can assert episodesCreated (§13).
func (db *DB) CountEpisodesForTask(task journal.TaskID) (n int, err error) {
	scope, err := db.bindJournalScope(context.Background(), projectionTargetLive)
	if err != nil {
		return 0, fmt.Errorf("CountEpisodesForTask: lease connection: %w", err)
	}
	defer scope.release()
	endTx := sqlitex.Save(scope.conn)
	defer endTx(&err)
	if err := sqlitex.Execute(scope.conn, "SELECT COUNT(*) FROM journal_authority_assignment_episodes WHERE task_id = ?1", &sqlitex.ExecOptions{Args: []any{task.String()}, ResultFunc: func(stmt *zs.Stmt) error { n = stmt.ColumnInt(0); return nil }}); err != nil {
		return 0, fmt.Errorf("CountEpisodesForTask %q: %w", task, err)
	}
	return n, nil
}
