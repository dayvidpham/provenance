package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/dayvidpham/provenance/internal/journal"
)

// expectedTable is one journal relation whose exact shape preflight enforces.
type expectedTable struct {
	name    string
	columns []string
}

// expectedJournalShape is the exact core journal shape this build understands.
var expectedJournalShape = []expectedTable{
	{"journal", []string{"journal_id", "kind_id", "actor_id", "recorded_at", "produced_by_operation_journal_id"}},
	{"journal_task_events", []string{"journal_id", "task_id", "event_kind", "payload"}},
	{"journal_operations", []string{"journal_id", "operation_id", "authority_journal_id", "command_digest", "mutation_digest", "mutation_encoding_version", "canonical_mutation"}},
	{"journal_authorities", []string{"journal_id", "authority_kind_id", "operation_authority_id"}},
}

// recognizedJournalSpineTables is the closed journal-prefixed relation set.
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
	"journal_decision_contexts":                {},
	"journal_evidence":                         {},
	"journal_evidence_contexts":                {},
	"journal_activity_creations":               {},
}

// PreflightSchema validates the journal shape on one pinned read transaction.
func (db *DB) PreflightSchema() error {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("PreflightSchema: lease connection: %w", err)
	}
	defer scope.release()
	return runScopedTransaction(scope.ctx, scope.conn, "BEGIN", scope.preflightSchema)
}

func (scope *connScope) preflightSchema() error {
	for _, want := range expectedJournalShape {
		present, err := scope.tableExists(want.name)
		if err != nil {
			return err
		}
		if !present {
			return &journal.SchemaPreflightError{
				Operation: "PreflightSchema", ExpectedShape: "table " + want.name,
				FoundShape: "table " + want.name + " absent", Stage: "table-presence check for " + want.name,
				Why:    "the live schema is missing a table this build requires before migration/replay can proceed",
				Impact: "no row of any kind is written; activation halts before any write transaction opens",
				Fix:    "restore the expected schema shape or run the correct forward migration, then re-open",
			}
		}
		actual, err := scope.tableColumns(want.name)
		if err != nil {
			return err
		}
		if want.name == "journal_operations" && isLegacyOperationsColumnSet(actual) {
			continue
		}
		if err := checkColumns(want, actual); err != nil {
			return err
		}
	}
	return scope.preflightNoUnexpectedSpineTable()
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

func (scope *connScope) preflightNoUnexpectedSpineTable() error {
	rows, err := scope.conn.QueryContext(scope.ctx, `SELECT name FROM sqlite_master
		WHERE type=?1 AND (name=?2 OR name LIKE ?3 ESCAPE ?4) ORDER BY name ASC`, "table", "journal", `journal\_%`, `\`)
	if err != nil {
		return fmt.Errorf("preflight: enumerate journal-spine tables: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("preflight: scan journal-spine table: %w", err)
		}
		if _, ok := recognizedJournalSpineTables[name]; !ok {
			return &journal.SchemaPreflightError{
				Operation: "PreflightSchema", ExpectedShape: "only recognized journal-spine tables present",
				FoundShape: "unexpected extra table " + name, Stage: "table-exclusivity check across the journal-spine set",
				Why:    "the live schema carries a journal-spine-named table outside any migration this build recognizes, so its shape is not fully understood",
				Impact: "no row of any kind is written; activation halts rather than proceeding against an unreviewed schema",
				Fix:    "revert the out-of-band table addition or teach this build the migration that added it, then re-open",
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("preflight: enumerate journal-spine tables: %w", err)
	}
	return nil
}

func checkColumns(want expectedTable, actual map[string]struct{}) error {
	expected := make(map[string]struct{}, len(want.columns))
	for _, column := range want.columns {
		expected[column] = struct{}{}
		if _, ok := actual[column]; !ok {
			return &journal.SchemaPreflightError{
				Operation: "PreflightSchema", ExpectedShape: fmt.Sprintf("column %s.%s", want.name, column),
				FoundShape: fmt.Sprintf("column %s.%s absent", want.name, column), Stage: fmt.Sprintf("column-presence check for %s.%s", want.name, column),
				Why:    "the live schema is missing an expected column this build reads",
				Impact: "no row of any kind is written; activation halts before any write transaction opens",
				Fix:    "restore the expected column or run the correct forward migration, then re-open",
			}
		}
	}
	for column := range actual {
		if _, ok := expected[column]; !ok {
			return &journal.SchemaPreflightError{
				Operation: "PreflightSchema", ExpectedShape: fmt.Sprintf("table %s with exactly its known columns", want.name),
				FoundShape: fmt.Sprintf("unexpected extra column %s.%s", want.name, column), Stage: fmt.Sprintf("column-exclusivity check for %s", want.name),
				Why:    "the live schema carries a column outside any migration this build recognizes, so its shape is not fully understood",
				Impact: "no row of any kind is written; activation halts rather than proceeding against an unreviewed schema",
				Fix:    "revert the out-of-band schema change or teach this build the migration that added it, then re-open",
			}
		}
	}
	return nil
}

func (scope *connScope) tableExists(table string) (bool, error) {
	var present bool
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type=?1 AND name=?2)", "table", table).Scan(&present); err != nil {
		return false, fmt.Errorf("preflight: probe table %q: %w", table, err)
	}
	return present, nil
}

func (scope *connScope) tableColumns(table string) (map[string]struct{}, error) {
	rows, err := scope.conn.QueryContext(scope.ctx, "SELECT name FROM pragma_table_info(?1)", table)
	if err != nil {
		return nil, fmt.Errorf("preflight: read columns of %q: %w", table, err)
	}
	defer rows.Close()
	columns := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("preflight: scan columns of %q: %w", table, err)
		}
		columns[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("preflight: read columns of %q: %w", table, err)
	}
	return columns, nil
}

// MigrateLegacyBaseline migrates deterministic legacy baselines atomically per
// batch, then re-tightens the task watermark once every row is anchored.
func (db *DB) MigrateLegacyBaseline(in journal.MigrationInput) (journal.MigrationResult, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return journal.MigrationResult{}, fmt.Errorf("MigrateLegacyBaseline: lease connection: %w", err)
	}
	defer scope.release()
	return scope.migrateWithFault(in, nil)
}

func (scope *connScope) migrateWithFault(in journal.MigrationInput, faultHook func(taskIndex int) error) (journal.MigrationResult, error) {
	if err := scope.preflightSchema(); err != nil {
		return journal.MigrationResult{}, err
	}
	if err := scope.ensureTasksWatermarkColumn(); err != nil {
		return journal.MigrationResult{}, err
	}
	ordered := append([]journal.LegacyTaskRow(nil), in.Legacy...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
		}
		return ordered[i].ID.String() < ordered[j].ID.String()
	})
	resolved := make([]*journal.ActorID, len(ordered))
	for i, legacy := range ordered {
		if legacy.RawOwner == "" {
			continue
		}
		owner, ok := in.Owners[legacy.RawOwner]
		if !ok {
			return journal.MigrationResult{}, &journal.MigrationOwnerUnmappableError{
				Operation: "MigrateLegacyBaseline", Task: legacy.ID, RawOwner: legacy.RawOwner,
				Stage:  "owner resolution, before any baseline row for the run is committed",
				Why:    "the legacy owner string resolved to no registered ActorID",
				Impact: "the entire migration transaction is rolled back; zero baselines are committed for any task in the run",
				Fix:    "register the owner as an actor, or correct/remove the legacy owner string, then re-run (the deterministic OperationID scheme makes the re-run idempotent)",
			}
		}
		resolved[i] = &owner
	}
	result, err := scope.anchorLegacyBaselines(in, ordered, resolved, faultHook)
	if err != nil {
		return journal.MigrationResult{}, err
	}
	unanchored, err := scope.countUnanchoredTasks()
	if err != nil {
		return journal.MigrationResult{}, err
	}
	if unanchored == 0 {
		if err := scope.rebuildTasksWatermark(tasksWatermarkNative); err != nil {
			return journal.MigrationResult{}, fmt.Errorf("MigrateLegacyBaseline: re-tighten tasks.last_journal_id to NOT NULL: %w", err)
		}
	}
	return result, nil
}

func (scope *connScope) anchorLegacyBaselines(in journal.MigrationInput, ordered []journal.LegacyTaskRow, resolved []*journal.ActorID, faultHook func(taskIndex int) error) (result journal.MigrationResult, err error) {
	result.TasksMigrated = len(ordered)
	err = runScopedTransaction(scope.ctx, scope.conn, "BEGIN", func() error {
		for i, legacy := range ordered {
			op := baselineOperation(in, legacy, resolved[i])
			if err := validateApplyInput(op); err != nil {
				return err
			}
			if err := assertHonestBaselineTimestamps(legacy, op); err != nil {
				return err
			}
			folded, err := scope.foldOperation(op, nil)
			if err != nil {
				return fmt.Errorf("MigrateLegacyBaseline: baseline for task %s: %w", legacy.ID.String(), err)
			}
			if folded.ShortCircuited {
				result.ShortCircuited++
			} else {
				result.BaselineAnchorsCreated++
			}
			if faultHook != nil {
				if err := faultHook(i); err != nil {
					return fmt.Errorf("%w: migration faulted after baseline %d of %d — why: the injected fault interrupted the all-or-nothing baseline batch; where: anchorLegacyBaselines migration transaction; when: after baseline folding and before transaction commit; impact: every baseline in this migration run is rolled back atomically; fix: resolve the fault and re-run the complete migration: %w", journal.ErrMigrationFault, i, len(ordered), err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return journal.MigrationResult{}, err
	}
	return result, nil
}

func baselineOperation(in journal.MigrationInput, legacy journal.LegacyTaskRow, owner *journal.ActorID) journal.OperationInput {
	authority := in.BootstrapAuthority
	updatedAt := legacy.UpdatedAt.UTC().UnixNano()
	op := journal.OperationInput{
		OperationID: journal.MigrationBaselineOperationID(legacy.ID), ActorID: in.System, AuthorityJournalID: &authority,
		CommandDigest:  []byte("provenance.migration.command--" + legacy.ID.String()),
		MutationDigest: []byte("provenance.migration.mutation--" + legacy.ID.String()), RecordedAt: updatedAt,
	}
	updated := updatedAt
	op.Effects = append(op.Effects, journal.Effect{Sort: journal.EffectTaskEvent, TaskID: legacy.ID, EventKind: journal.EventKindTaskMigrated, Payload: journal.EncodeMigrationMarkerPayload(legacy.Status), RecordedAtOverride: &updated})
	if owner == nil {
		return op
	}
	assignment := journal.MigrationBaselineAssignmentID(legacy.ID)
	op.Effects = append(op.Effects, journal.Effect{Sort: journal.EffectAssignmentStart, AssignmentID: assignment, TaskID: legacy.ID, SlotID: journal.SlotOwnerResponsibility, Occupant: *owner, RecordedAtOverride: &updated})
	if legacy.Status == journal.TaskStatusClosed {
		endedAt := updatedAt
		if legacy.ClosedAt != nil {
			endedAt = legacy.ClosedAt.UTC().UnixNano()
		}
		ended := endedAt
		op.Effects = append(op.Effects, journal.Effect{Sort: journal.EffectAssignmentEnd, AssignmentID: assignment, TaskID: legacy.ID, SlotID: journal.SlotOwnerResponsibility, RecordedAtOverride: &ended})
	}
	return op
}

func assertHonestBaselineTimestamps(legacy journal.LegacyTaskRow, op journal.OperationInput) error {
	legal := map[int64]struct{}{legacy.UpdatedAt.UTC().UnixNano(): {}}
	if legacy.ClosedAt != nil {
		legal[legacy.ClosedAt.UTC().UnixNano()] = struct{}{}
	}
	for i := range op.Effects {
		timestamp := op.Effects[i].RecordedAtOverride
		if timestamp == nil {
			continue
		}
		if _, ok := legal[*timestamp]; !ok {
			return fmt.Errorf("%w: legacy task %s effect %d has RecordedAt %d not drawn from legacy updated_at/closed_at; fix: use only the legacy row timestamps", journal.ErrDishonestMigrationTimestamp, legacy.ID.String(), i, *timestamp)
		}
	}
	return nil
}

func (db *DB) CountBaselineAnchors() (int, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return 0, fmt.Errorf("CountBaselineAnchors: lease connection: %w", err)
	}
	defer scope.release()
	var count int
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM journal_operations WHERE operation_id LIKE ?1", "provenance.migration.baseline--%").Scan(&count); err != nil {
		return 0, fmt.Errorf("CountBaselineAnchors: %w", err)
	}
	return count, nil
}

func (db *DB) EpisodeTransitionRecordedAt(assignment journal.AssignmentID, started bool) (int64, bool, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return 0, false, fmt.Errorf("EpisodeTransitionRecordedAt: lease connection: %w", err)
	}
	defer scope.release()
	transition := transitionEndedID
	if started {
		transition = transitionStartedID
	}
	var timestamp int64
	if err := scope.conn.QueryRowContext(scope.ctx, `SELECT j.recorded_at FROM journal_authority_assignment_transitions t
		JOIN journal j ON j.journal_id=t.journal_id WHERE t.assignment_id=?1 AND t.transition_id=?2`, string(assignment), transition).Scan(&timestamp); err != nil {
		if err == sql.ErrNoRows {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("EpisodeTransitionRecordedAt %q: %w", assignment, err)
	}
	return timestamp, true, nil
}

func (db *DB) EpisodeActive(assignment journal.AssignmentID) (bool, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return false, fmt.Errorf("EpisodeActive: lease connection: %w", err)
	}
	defer scope.release()
	started, err := scope.transitionExists(assignment, transitionStartedID)
	if err != nil {
		return false, err
	}
	ended, err := scope.transitionExists(assignment, transitionEndedID)
	if err != nil {
		return false, err
	}
	return started && !ended, nil
}

func (db *DB) CountEpisodesForTask(task journal.TaskID) (int, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return 0, fmt.Errorf("CountEpisodesForTask: lease connection: %w", err)
	}
	defer scope.release()
	var count int
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM journal_authority_assignment_episodes WHERE task_id=?1", task.String()).Scan(&count); err != nil {
		return 0, fmt.Errorf("CountEpisodesForTask %q: %w", task, err)
	}
	return count, nil
}
