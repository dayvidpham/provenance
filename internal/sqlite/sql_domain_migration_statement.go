package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type migrationStatement uint16

const (
	migrationInsertTasks879a migrationStatement = iota + 1
	migrationInsertTasksWatermarkRebuildd091
	migrationInsertTasksWatermarkRebuilddc9f
	migrationPragmaTableInfo6558
	migrationSelectJournalAuthorityAssignmentEpisodes1720
	migrationSelectJournalAuthorityAssignmentTransitions2a64
	migrationSelectJournalOperations1194
	migrationSelectPragmaTableInfo94bb
	migrationSelectSqliteMaster7370
	migrationSelectTasks0c06
)

func (migrationStatement) statementClass() sqlStatementClass { return sqlDMLStatement }

func (statement migrationStatement) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case migrationInsertTasks879a:
		return sqlitex.Execute(conn, "INSERT INTO tasks\n\t\t\t(id, namespace, title, description, status_id, priority_id, type_id,\n\t\t\t phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14)", options)
	case migrationInsertTasksWatermarkRebuildd091:
		return sqlitex.Execute(conn, "INSERT INTO tasks_watermark_rebuild (id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason) SELECT id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason FROM tasks", options)
	case migrationInsertTasksWatermarkRebuilddc9f:
		return sqlitex.Execute(conn, "INSERT INTO tasks_watermark_rebuild (id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id) SELECT id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id FROM tasks", options)
	case migrationPragmaTableInfo6558:
		return sqlitex.Execute(conn, "PRAGMA table_info(tasks)", options)
	case migrationSelectJournalAuthorityAssignmentEpisodes1720:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal_authority_assignment_episodes WHERE task_id = ?1", options)
	case migrationSelectJournalAuthorityAssignmentTransitions2a64:
		return sqlitex.Execute(conn, "SELECT j.recorded_at FROM journal_authority_assignment_transitions t\n\t\t JOIN journal j ON j.journal_id = t.journal_id\n\t\t WHERE t.assignment_id = ?1 AND t.transition_id = ?2", options)
	case migrationSelectJournalOperations1194:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal_operations WHERE operation_id LIKE ?1", options)
	case migrationSelectPragmaTableInfo94bb:
		return sqlitex.Execute(conn, "SELECT name FROM pragma_table_info(?1)", options)
	case migrationSelectSqliteMaster7370:
		return sqlitex.Execute(conn, "SELECT name FROM sqlite_master WHERE type=?1\n\t\t   AND (name = ?2 OR name LIKE ?3 ESCAPE ?4)\n\t\t ORDER BY name ASC", options)
	case migrationSelectTasks0c06:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM tasks WHERE last_journal_id IS ?1", appendStaticSQLArgs(options, nil))
	default:
		return unknownSQLStatementError("migrationStatement", uint16(statement))
	}
}
