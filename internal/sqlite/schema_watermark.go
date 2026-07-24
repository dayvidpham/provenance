package sqlite

import (
	"context"
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

func (shape tasksWatermarkShape) createRebuildQuery() string {
	switch shape {
	case tasksWatermarkNative:
		return "CREATE TABLE tasks_watermark_rebuild (id TEXT PRIMARY KEY,namespace TEXT NOT NULL,title TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',status_id INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),priority_id INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id),type_id INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),phase_id INTEGER NOT NULL REFERENCES phases(id),owner_id TEXT REFERENCES agents(id),notes TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,closed_at INTEGER,close_reason TEXT NOT NULL DEFAULT '',last_journal_id INTEGER NOT NULL REFERENCES journal(journal_id)) STRICT"
	case tasksWatermarkNullable:
		return "CREATE TABLE tasks_watermark_rebuild (id TEXT PRIMARY KEY,namespace TEXT NOT NULL,title TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',status_id INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),priority_id INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id),type_id INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),phase_id INTEGER NOT NULL REFERENCES phases(id),owner_id TEXT REFERENCES agents(id),notes TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,closed_at INTEGER,close_reason TEXT NOT NULL DEFAULT '',last_journal_id INTEGER REFERENCES journal(journal_id)) STRICT"
	case tasksWatermarkColumnless:
		return "CREATE TABLE tasks_watermark_rebuild (id TEXT PRIMARY KEY,namespace TEXT NOT NULL,title TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',status_id INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),priority_id INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id),type_id INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),phase_id INTEGER NOT NULL REFERENCES phases(id),owner_id TEXT REFERENCES agents(id),notes TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,closed_at INTEGER,close_reason TEXT NOT NULL DEFAULT '') STRICT"
	default:
		panic("unknown tasks watermark shape")
	}
}

func (shape tasksWatermarkShape) copyQuery() string {
	switch shape {
	case tasksWatermarkNative, tasksWatermarkNullable:
		return "INSERT INTO tasks_watermark_rebuild (id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id) SELECT id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id FROM tasks"
	case tasksWatermarkColumnless:
		return "INSERT INTO tasks_watermark_rebuild (id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason) SELECT id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason FROM tasks"
	default:
		panic("unknown tasks watermark shape")
	}
}

// tasksWatermarkColumnInfoLocked reports whether the tasks table has a last_journal_id
// column and, if so, whether it is declared NOT NULL. Assumes db.conn is operation-owned.
func (db *connScope) tasksWatermarkColumnInfoLocked() (present bool, notNull bool, err error) {
	if err := sqlitex.Execute(db.conn, "PRAGMA table_info(tasks)", &sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
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

// P2 compatibility adapter for existing internal schema assertions. The test
// caller owns the reserved connection; production migration paths use connScope.
func (db *DB) tasksWatermarkColumnInfoLocked() (present bool, notNull bool, err error) {
	return borrowConnScope(db.conn, db.projectionTarget).tasksWatermarkColumnInfoLocked()
}

// countUnanchoredTasksLocked reports how many tasks rows still carry a NULL watermark —
// i.e. legacy rows not yet journal-anchored (§8.1, §13). Migration consults it to decide
// whether the whole table can be re-tightened to NOT NULL: the rebuild is only safe (and
// only meaningful) when zero un-anchored rows remain. A no-op when the column is absent
// (the even-older column-less legacy shape) — a column-less table has no watermark to be
// null, and migration adds the column before anchoring, so this runs after that add.
// Assumes db.conn is operation-owned.
func (db *connScope) countUnanchoredTasksLocked() (int, error) {
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
	if err := sqlitex.Execute(db.conn, "SELECT COUNT(*) FROM tasks WHERE last_journal_id IS ?1", &sqlitex.ExecOptions{Args: []any{nil}, ResultFunc: func(stmt *zs.Stmt) error { n = stmt.ColumnInt(0); return nil }}); err != nil {
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
// rebuild back. Assumes db.conn is operation-owned.
func (db *connScope) rebuildTasksWatermarkLocked(shape tasksWatermarkShape) error {
	if err := sqlitex.ExecuteTransient(db.conn, "PRAGMA foreign_keys=OFF", nil); err != nil {
		return fmt.Errorf("rebuildTasksWatermark: disable FK enforcement: %w", err)
	}
	defer func() { _ = sqlitex.ExecuteTransient(db.conn, "PRAGMA foreign_keys=ON", nil) }()

	if err := sqlitex.ExecuteTransient(db.conn, "BEGIN IMMEDIATE", nil); err != nil {
		return fmt.Errorf("rebuildTasksWatermark: begin: %w", err)
	}
	if err := sqlitex.ExecuteTransient(db.conn, shape.createRebuildQuery(), nil); err != nil {
		_ = sqlitex.ExecuteTransient(db.conn, "ROLLBACK", nil)
		return fmt.Errorf("rebuildTasksWatermark: create rebuild table: %w", err)
	}
	if err := sqlitex.Execute(db.conn, shape.copyQuery(), nil); err != nil {
		_ = sqlitex.ExecuteTransient(db.conn, "ROLLBACK", nil)
		return fmt.Errorf("rebuildTasksWatermark: copy rows: %w", err)
	}
	if err := sqlitex.ExecuteTransient(db.conn, "DROP TABLE tasks", nil); err != nil {
		_ = sqlitex.ExecuteTransient(db.conn, "ROLLBACK", nil)
		return fmt.Errorf("rebuildTasksWatermark: drop old table: %w", err)
	}
	if err := sqlitex.ExecuteTransient(db.conn, "ALTER TABLE tasks_watermark_rebuild RENAME TO tasks", nil); err != nil {
		_ = sqlitex.ExecuteTransient(db.conn, "ROLLBACK", nil)
		return fmt.Errorf("rebuildTasksWatermark: rename rebuilt table: %w", err)
	}
	// Recreate the indexes dropped with the old tasks table.
	for _, ddl := range []string{
		"CREATE INDEX IF NOT EXISTS idx_tasks_namespace ON tasks (namespace)",
		"CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks (status_id)",
		"CREATE INDEX IF NOT EXISTS idx_tasks_priority ON tasks (priority_id)",
		"CREATE INDEX IF NOT EXISTS idx_tasks_type ON tasks (type_id)",
		"CREATE INDEX IF NOT EXISTS idx_tasks_phase ON tasks (phase_id)",
		"CREATE INDEX IF NOT EXISTS idx_tasks_owner ON tasks (owner_id)",
	} {
		if err := sqlitex.ExecuteTransient(db.conn, ddl, nil); err != nil {
			_ = sqlitex.ExecuteTransient(db.conn, "ROLLBACK", nil)
			return fmt.Errorf("rebuildTasksWatermark: recreate index: %w", err)
		}
	}
	var violations int
	if err := sqlitex.ExecuteTransient(db.conn, "PRAGMA foreign_key_check",
		&sqlitex.ExecOptions{ResultFunc: func(*zs.Stmt) error { violations++; return nil }}); err != nil {
		_ = sqlitex.ExecuteTransient(db.conn, "ROLLBACK", nil)
		return fmt.Errorf("rebuildTasksWatermark: foreign_key_check: %w", err)
	}
	if violations > 0 {
		_ = sqlitex.ExecuteTransient(db.conn, "ROLLBACK", nil)
		return fmt.Errorf(
			"rebuildTasksWatermark: rebuild left %d foreign-key violations, rolled back — where: tasks "+
				"watermark rebuild; impact: the rebuild was reverted and the database left unchanged; fix: "+
				"this indicates a child row (edge/label/comment) references a task id that does not exist",
			violations)
	}
	if err := sqlitex.ExecuteTransient(db.conn, "COMMIT", nil); err != nil {
		_ = sqlitex.ExecuteTransient(db.conn, "ROLLBACK", nil)
		return fmt.Errorf("rebuildTasksWatermark: commit rebuild: %w", err)
	}
	return nil
}

// downgradeTasksWatermarkToLegacyLocked relaxes the tasks table to the legacy (pre-#5)
// nullable-watermark shape so a legacy row can be seeded with no watermark (§13). It is
// the "old-schema downgrade" the test seeding seams perform: a no-op when the watermark
// is already nullable or the column is already absent, a rebuild-to-nullable when the
// live schema is the tightened NOT NULL shape. Existing rows (including any native rows
// already carrying a watermark) are preserved. Assumes db.conn is operation-owned.
func (db *connScope) downgradeTasksWatermarkToLegacyLocked() error {
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
	scope, err := db.bindJournalScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("DowngradeTasksToColumnlessLegacy: lease connection: %w", err)
	}
	defer scope.release()
	present, _, err := scope.tasksWatermarkColumnInfoLocked()
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	return scope.rebuildTasksWatermarkLocked(tasksWatermarkColumnless)
}

// ensureTasksWatermarkColumnLocked is migration's column-add path (§13): a legacy
// database that predates the last_journal_id column entirely gets it added (nullable,
// with the journal FK) before any legacy row is anchored, so the anchoring projection
// has a column to write the watermark into. Idempotent: a no-op when the column already
// exists (the common case, where the legacy nullable column is present). Assumes
// db.conn is operation-owned.
func (db *connScope) ensureTasksWatermarkColumnLocked() error {
	present, _, err := db.tasksWatermarkColumnInfoLocked()
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	if err := sqlitex.ExecuteTransient(db.conn,
		"ALTER TABLE tasks ADD COLUMN last_journal_id INTEGER REFERENCES journal(journal_id)", nil); err != nil {
		return fmt.Errorf(
			"ensureTasksWatermarkColumn: add legacy last_journal_id column — where: migration column-add "+
				"path (§13); when: before any legacy row is anchored; impact: nothing committed; fix: the "+
				"legacy tasks table shape is not understood: %w", err)
	}
	return nil
}
