package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type migrationDDL uint16

const (
	migrationDDLAlterTasksWatermarkRebuild6df4 migrationDDL = iota + 1
	migrationDDLAlterTasksed3d
	migrationDDLCreateIdxTasksOwner2af7
	migrationDDLCreateIdxTasksPhase8793
	migrationDDLCreateIdxTasksPriority4dc7
	migrationDDLCreateIdxTasksStatus0073
	migrationDDLCreateIdxTasksTyped2dc
	migrationDDLCreateTasksWatermarkRebuild6df2
	migrationDDLCreateTasksWatermarkRebuild865e
	migrationDDLCreateTasksWatermarkRebuilde9e2
	migrationDDLDropTasks7ba0
)

func (migrationDDL) statementClass() sqlStatementClass { return sqlDDLStatement }

func (statement migrationDDL) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case migrationDDLAlterTasksWatermarkRebuild6df4:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE tasks_watermark_rebuild RENAME TO tasks", options)
	case migrationDDLAlterTasksed3d:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE tasks ADD COLUMN last_journal_id INTEGER REFERENCES journal(journal_id)", options)
	case migrationDDLCreateIdxTasksOwner2af7:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_owner ON tasks (owner_id)", options)
	case migrationDDLCreateIdxTasksPhase8793:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_phase ON tasks (phase_id)", options)
	case migrationDDLCreateIdxTasksPriority4dc7:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_priority ON tasks (priority_id)", options)
	case migrationDDLCreateIdxTasksStatus0073:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks (status_id)", options)
	case migrationDDLCreateIdxTasksTyped2dc:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_type ON tasks (type_id)", options)
	case migrationDDLCreateTasksWatermarkRebuild6df2:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE tasks_watermark_rebuild (id TEXT PRIMARY KEY,namespace TEXT NOT NULL,title TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',status_id INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),priority_id INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id),type_id INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),phase_id INTEGER NOT NULL REFERENCES phases(id),owner_id TEXT REFERENCES agents(id),notes TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,closed_at INTEGER,close_reason TEXT NOT NULL DEFAULT '',last_journal_id INTEGER REFERENCES journal(journal_id)) STRICT", options)
	case migrationDDLCreateTasksWatermarkRebuild865e:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE tasks_watermark_rebuild (id TEXT PRIMARY KEY,namespace TEXT NOT NULL,title TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',status_id INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),priority_id INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id),type_id INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),phase_id INTEGER NOT NULL REFERENCES phases(id),owner_id TEXT REFERENCES agents(id),notes TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,closed_at INTEGER,close_reason TEXT NOT NULL DEFAULT '') STRICT", options)
	case migrationDDLCreateTasksWatermarkRebuilde9e2:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE tasks_watermark_rebuild (id TEXT PRIMARY KEY,namespace TEXT NOT NULL,title TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',status_id INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),priority_id INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id),type_id INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),phase_id INTEGER NOT NULL REFERENCES phases(id),owner_id TEXT REFERENCES agents(id),notes TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,closed_at INTEGER,close_reason TEXT NOT NULL DEFAULT '',last_journal_id INTEGER NOT NULL REFERENCES journal(journal_id)) STRICT", options)
	case migrationDDLDropTasks7ba0:
		return sqlitex.ExecuteTransient(conn, "DROP TABLE tasks", options)
	default:
		return unknownSQLStatementError("migrationDDL", uint16(statement))
	}
}
