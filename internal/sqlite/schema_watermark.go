package sqlite

import (
	"fmt"

	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// schema_watermark.go owns the tasks.last_journal_id watermark schema and the
// transforms between its two shapes (docs/journal-relational-contract.md §8.1, §13).
//
// A FRESH native database ships the tightened #5 shape: last_journal_id is NOT NULL,
// so every task row reflects a journal position by construction — the only production
// tasks-row INSERT is the reducer fold, which always carries the watermark. A LEGACY
// database predates the tightening (the journal-base #4 shape had a NULLABLE watermark,
// and an even older database may lack the column entirely); such a database is upgraded
// by MigrateLegacyBaseline, which — if needed — adds the column first, anchors every
// legacy row (populating each watermark), then RE-TIGHTENS the column back to NOT NULL
// so a migrated database converges to the exact same schema-level enforcement a fresh
// database ships (the enforcement is structural post-migration, not merely data-level).
// The test seams simulate legacy databases by DOWNGRADING a fresh native schema to the
// legacy nullable shape (a row can then be seeded with no watermark), exactly mirroring a
// pre-tightening database on disk (§13 CORPUS/TEST PATH); post-migration the schema is
// tight again, so a fresh downgrade seam is what a later test uses to model new legacy
// state.

type tasksWatermarkShape uint8

const (
	tasksWatermarkNative tasksWatermarkShape = iota + 1
	tasksWatermarkNullable
	tasksWatermarkColumnless
)

func (shape tasksWatermarkShape) createRebuildStatement() sealedSQLStatement {
	switch shape {
	case tasksWatermarkNative:
		return migrationDDLCreateTasksWatermarkRebuilde9e2
	case tasksWatermarkNullable:
		return migrationDDLCreateTasksWatermarkRebuild6df2
	case tasksWatermarkColumnless:
		return migrationDDLCreateTasksWatermarkRebuild865e
	default:
		panic("unknown tasks watermark shape")
	}
}

func (shape tasksWatermarkShape) copyStatement() sealedSQLStatement {
	switch shape {
	case tasksWatermarkNative, tasksWatermarkNullable:
		return migrationInsertTasksWatermarkRebuilddc9f
	case tasksWatermarkColumnless:
		return migrationInsertTasksWatermarkRebuildd091
	default:
		panic("unknown tasks watermark shape")
	}
}

// tasksIndexDDL returns the CREATE INDEX statements for the tasks table. They are
// recreated after every watermark rebuild because the rebuild drops and renames the
// table (the indexes go with the dropped table).
func tasksIndexDDL() []sealedSQLStatement {
	return []sealedSQLStatement{
		sharedDDLCreateIdxTasksNamespace7486,
		migrationDDLCreateIdxTasksStatus0073,
		migrationDDLCreateIdxTasksPriority4dc7,
		migrationDDLCreateIdxTasksTyped2dc,
		migrationDDLCreateIdxTasksPhase8793,
		migrationDDLCreateIdxTasksOwner2af7,
	}
}

// tasksWatermarkColumnInfoLocked reports whether the tasks table has a last_journal_id
// column and, if so, whether it is declared NOT NULL. Assumes db.mu is held.
func (db *DB) tasksWatermarkColumnInfoLocked() (present bool, notNull bool, err error) {
	if err := executeStatement(db.conn,
		migrationPragmaTableInfo6558,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			if stmt.ColumnText(1) == "last_journal_id" {
				present = true
				notNull = stmt.ColumnInt(3) == 1 // column 3 of table_info is "notnull"
			}
			return nil
		}}); err != nil {
		return false, false, fmt.Errorf("tasksWatermarkColumnInfo: %w", err)
	}
	return present, notNull, nil
}

// countUnanchoredTasksLocked reports how many tasks rows still carry a NULL watermark —
// i.e. legacy rows not yet journal-anchored (§8.1, §13). Migration consults it to decide
// whether the whole table can be re-tightened to NOT NULL: the rebuild is only safe (and
// only meaningful) when zero un-anchored rows remain. A no-op when the column is absent
// (the even-older column-less legacy shape) — a column-less table has no watermark to be
// null, and migration adds the column before anchoring, so this runs after that add.
// Assumes db.mu is held.
func (db *DB) countUnanchoredTasksLocked() (int, error) {
	present, _, err := db.tasksWatermarkColumnInfoLocked()
	if err != nil {
		return 0, err
	}
	if !present {
		return 0, fmt.Errorf(
			"countUnanchoredTasks: tasks table has no last_journal_id column — where: migration " +
				"re-tightening gate (§8.1, §13); when: after the column-add path should have run; impact: " +
				"nothing re-tightened; fix: this indicates the column-add path was skipped, which is a bug")
	}
	var n int
	if err := executeStatement(db.conn,
		migrationSelectTasks0c06,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error { n = stmt.ColumnInt(0); return nil }}); err != nil {
		return 0, fmt.Errorf("countUnanchoredTasks: %w", err)
	}
	return n, nil
}

// rebuildTasksWatermarkLocked runs the canonical SQLite table rebuild to change the
// tasks table to the requested watermark shape (§13). It preserves every row, the child
// foreign keys that reference tasks(id) (edges/labels/comments, which reference by name
// across the drop+rename), and the tasks indexes. copyWatermark controls whether the
// existing last_journal_id values are carried into the rebuilt table (false when the old
// table has no such column). FK enforcement is toggled around an explicit transaction
// exactly as completeJournalOperationFK does; a detected violation rolls the whole
// rebuild back. Assumes db.mu is held.
func (db *DB) rebuildTasksWatermarkLocked(shape tasksWatermarkShape) error {
	if err := executeStatement(db.conn, sharedDDLPragmaForeignKeys1be4, nil); err != nil {
		return fmt.Errorf("rebuildTasksWatermark: disable FK enforcement: %w", err)
	}
	defer func() { _ = executeStatement(db.conn, sharedDDLPragmaForeignKeysde7c, nil) }()

	steps := []sealedSQLStatement{
		sharedDDLBeginStatement4e51,
		shape.createRebuildStatement(),
		shape.copyStatement(),
		migrationDDLDropTasks7ba0,
		migrationDDLAlterTasksWatermarkRebuild6df4,
	}
	steps = append(steps, tasksIndexDDL()...)
	for _, stmt := range steps {
		if err := executeStatement(db.conn, stmt, nil); err != nil {
			_ = executeStatement(db.conn, sharedDDLRollbackStatement4eec, nil)
			return fmt.Errorf("rebuildTasksWatermark: static step failed: %w", err)
		}
	}
	var violations int
	if err := executeStatement(db.conn, sharedDDLPragmaForeignKeyCheck6847,
		&sqlitex.ExecOptions{ResultFunc: func(*zs.Stmt) error { violations++; return nil }}); err != nil {
		_ = executeStatement(db.conn, sharedDDLRollbackStatement4eec, nil)
		return fmt.Errorf("rebuildTasksWatermark: foreign_key_check: %w", err)
	}
	if violations > 0 {
		_ = executeStatement(db.conn, sharedDDLRollbackStatement4eec, nil)
		return fmt.Errorf(
			"rebuildTasksWatermark: rebuild left %d foreign-key violations, rolled back — where: tasks "+
				"watermark rebuild; impact: the rebuild was reverted and the database left unchanged; fix: "+
				"this indicates a child row (edge/label/comment) references a task id that does not exist",
			violations)
	}
	if err := executeStatement(db.conn, sharedDDLCommitStatement696a, nil); err != nil {
		_ = executeStatement(db.conn, sharedDDLRollbackStatement4eec, nil)
		return fmt.Errorf("rebuildTasksWatermark: commit rebuild: %w", err)
	}
	return nil
}

// downgradeTasksWatermarkToLegacyLocked relaxes the tasks table to the legacy (pre-#5)
// nullable-watermark shape so a legacy row can be seeded with no watermark (§13). It is
// the "old-schema downgrade" the test seeding seams perform: a no-op when the watermark
// is already nullable or the column is already absent, a rebuild-to-nullable when the
// live schema is the tightened NOT NULL shape. Existing rows (including any native rows
// already carrying a watermark) are preserved. Assumes db.mu is held.
func (db *DB) downgradeTasksWatermarkToLegacyLocked() error {
	present, notNull, err := db.tasksWatermarkColumnInfoLocked()
	if err != nil {
		return err
	}
	if !present || !notNull {
		return nil // already legacy-shaped (nullable, or the even-older column-less shape)
	}
	return db.rebuildTasksWatermarkLocked(tasksWatermarkNullable)
}

// DowngradeTasksToColumnlessLegacy rebuilds the tasks table to the even-older legacy
// shape that predates the last_journal_id column entirely (§13), so a test can drive
// MigrateLegacyBaseline's column-add path against a genuinely column-less database. Like
// SeedLegacyTask it is a narrow, test-only *DB seam that models a legacy database on
// disk, never part of the JournalAPI surface. Existing base-column rows are preserved.
func (db *DB) DowngradeTasksToColumnlessLegacy() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	present, _, err := db.tasksWatermarkColumnInfoLocked()
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	return db.rebuildTasksWatermarkLocked(tasksWatermarkColumnless)
}

// ensureTasksWatermarkColumnLocked is migration's column-add path (§13): a legacy
// database that predates the last_journal_id column entirely gets it added (nullable,
// with the journal FK) before any legacy row is anchored, so the anchoring projection
// has a column to write the watermark into. Idempotent: a no-op when the column already
// exists (the common case, where the legacy nullable column is present). Assumes db.mu
// is held.
func (db *DB) ensureTasksWatermarkColumnLocked() error {
	present, _, err := db.tasksWatermarkColumnInfoLocked()
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	if err := executeStatement(db.conn,
		migrationDDLAlterTasksed3d, nil); err != nil {
		return fmt.Errorf(
			"ensureTasksWatermarkColumn: add legacy last_journal_id column — where: migration column-add "+
				"path (§13); when: before any legacy row is anchored; impact: nothing committed; fix: the "+
				"legacy tasks table shape is not understood: %w", err)
	}
	return nil
}
