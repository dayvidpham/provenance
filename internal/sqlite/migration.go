package sqlite

import (
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
	{"journal_operations", []string{"journal_id", "operation_id", "authority_journal_id", "command_digest", "mutation_digest"}},
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
}

// PreflightSchema verifies the external pre-journal schema shape (§13). It is a
// read-only check that runs strictly before any transaction opens; any mismatch is
// a typed SchemaPreflightError and no row of any kind is written.
func (db *DB) PreflightSchema() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.preflightSchemaLocked()
}

func (db *DB) preflightSchemaLocked() error {
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
				Impact:        "no row of any kind is written; activation halts before any transaction opens",
				Fix:           "restore the expected schema shape or run the correct forward migration, then re-open",
			}
		}
		actual, err := db.tableColumnsLocked(want.name)
		if err != nil {
			return err
		}
		if perr := checkColumns(want, actual); perr != nil {
			return perr
		}
	}
	// Extra-table direction (§13): every live `journal`-prefixed table must be a
	// recognized journal-spine relation. An unrecognized one fails closed.
	return db.preflightNoUnexpectedSpineTableLocked()
}

// preflightNoUnexpectedSpineTableLocked enumerates every live table whose name
// follows the journal-spine convention (name = 'journal' or name LIKE 'journal_%')
// and fails closed with a typed SchemaPreflightError on any name outside the
// closed recognizedJournalSpineTables set — the extra-table direction §13's prose
// promises alongside the extra-column direction (§13, checkColumns).
func (db *DB) preflightNoUnexpectedSpineTableLocked() error {
	var unexpected string
	if err := sqlitex.Execute(db.conn,
		`SELECT name FROM sqlite_master WHERE type='table'
		   AND (name = 'journal' OR name LIKE 'journal\_%' ESCAPE '\')
		 ORDER BY name ASC`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
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
				Impact:        "no row of any kind is written; activation halts before any transaction opens",
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

func (db *DB) tableExistsLocked(table string) (bool, error) {
	present := false
	if err := sqlitex.Execute(db.conn,
		`SELECT 1 FROM sqlite_master WHERE type='table' AND name=?1`,
		&sqlitex.ExecOptions{Args: []any{table}, ResultFunc: func(*zs.Stmt) error { present = true; return nil }}); err != nil {
		return false, fmt.Errorf("preflight: probe table %q: %w", table, err)
	}
	return present, nil
}

func (db *DB) tableColumnsLocked(table string) (map[string]struct{}, error) {
	cols := map[string]struct{}{}
	// PRAGMA table_info cannot be parameterized; the table name comes from the
	// closed expectedJournalShape set, never from caller input.
	if err := sqlitex.Execute(db.conn,
		fmt.Sprintf(`PRAGMA table_info(%q)`, table),
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			cols[stmt.ColumnText(1)] = struct{}{} // column 1 is the column name
			return nil
		}}); err != nil {
		return nil, fmt.Errorf("preflight: read columns of %q: %w", table, err)
	}
	return cols, nil
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
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.migrateLockedWithFault(in, nil)
}

func (db *DB) migrateLockedWithFault(in journal.MigrationInput, faultHook func(taskIndex int) error) (journal.MigrationResult, error) {
	// Preflight strictly before any transaction opens (§13).
	if err := db.preflightSchemaLocked(); err != nil {
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

	var txErr error
	endTx := sqlitex.Save(db.conn) // outer whole-batch savepoint (§9.5, §13)
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
		res, err := db.applyLocked(op, nil)
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
func (db *DB) CountBaselineAnchors() (int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var n int
	if err := sqlitex.Execute(db.conn,
		`SELECT COUNT(*) FROM journal_operations WHERE operation_id LIKE 'provenance.migration.baseline--%'`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error { n = stmt.ColumnInt(0); return nil }}); err != nil {
		return 0, fmt.Errorf("CountBaselineAnchors: %w", err)
	}
	return n, nil
}

// StartedTransitionRecordedAt returns the RecordedAt (UnixNano) stamped on the
// started transition of a migration episode, so a corpus history can assert the
// honest legacy timestamp was used (§13). ok is false when no such episode exists.
func (db *DB) EpisodeTransitionRecordedAt(assignment journal.AssignmentID, started bool) (int64, bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	transition := transitionEndedID
	if started {
		transition = transitionStartedID
	}
	var recordedAt int64
	found := false
	if err := sqlitex.Execute(db.conn,
		`SELECT j.recorded_at FROM journal_authority_assignment_transitions t
		 JOIN journal j ON j.journal_id = t.journal_id
		 WHERE t.assignment_id = ?1 AND t.transition_id = ?2`,
		&sqlitex.ExecOptions{Args: []any{string(assignment), transition}, ResultFunc: func(stmt *zs.Stmt) error {
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
func (db *DB) EpisodeActive(assignment journal.AssignmentID) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	started, err := db.transitionExistsLocked(assignment, transitionStartedID)
	if err != nil {
		return false, err
	}
	ended, err := db.transitionExistsLocked(assignment, transitionEndedID)
	if err != nil {
		return false, err
	}
	return started && !ended, nil
}

// CountEpisodesForTask returns how many owner-responsibility episodes exist for a
// task, so a corpus history can assert episodesCreated (§13).
func (db *DB) CountEpisodesForTask(task journal.TaskID) (int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var n int
	if err := sqlitex.Execute(db.conn,
		`SELECT COUNT(*) FROM journal_authority_assignment_episodes WHERE task_id = ?1`,
		&sqlitex.ExecOptions{Args: []any{task.String()}, ResultFunc: func(stmt *zs.Stmt) error { n = stmt.ColumnInt(0); return nil }}); err != nil {
		return 0, fmt.Errorf("CountEpisodesForTask %q: %w", task, err)
	}
	return n, nil
}
