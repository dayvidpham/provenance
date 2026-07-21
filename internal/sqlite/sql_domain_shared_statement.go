package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type sharedStatement uint16

const (
	sharedInsertJournalAuthoritiesd41a sharedStatement = iota + 1
	sharedInsertJournalAuthorityAssignmentTransitions6b1d
	sharedInsertJournalAuthorityBootstrapsab65
	sharedInsertJournalTaskEventsf716
	sharedInsertJournale268
	sharedSelectJournalde83
	sharedSelectJournalef66
	sharedSelectSqliteMasterc39e
	sharedUpdateTasksf343
)

func (sharedStatement) statementClass() sqlStatementClass { return sqlDMLStatement }

func (statement sharedStatement) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case sharedInsertJournalAuthoritiesd41a:
		return sqlitex.Execute(conn, "INSERT INTO journal_authorities (journal_id, authority_kind_id, operation_authority_id) VALUES (?1, ?2, ?3)", options)
	case sharedInsertJournalAuthorityAssignmentTransitions6b1d:
		return sqlitex.Execute(conn, "INSERT INTO journal_authority_assignment_transitions (journal_id, assignment_id, transition_id) VALUES (?1, ?2, ?3)", options)
	case sharedInsertJournalAuthorityBootstrapsab65:
		return sqlitex.Execute(conn, "INSERT INTO journal_authority_bootstraps (journal_id, label) VALUES (?1, ?2)", options)
	case sharedInsertJournalTaskEventsf716:
		return sqlitex.Execute(conn, "INSERT INTO journal_task_events (journal_id, task_id, event_kind, payload) VALUES (?1, ?2, ?3, ?4)", options)
	case sharedInsertJournale268:
		return sqlitex.Execute(conn, "INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id) VALUES (?1, ?2, ?3, ?4)", options)
	case sharedSelectJournalde83:
		return sqlitex.Execute(conn, "SELECT COALESCE(MAX(journal_id), ?1) FROM journal", options)
	case sharedSelectJournalef66:
		return sqlitex.Execute(conn, "SELECT journal_id FROM journal WHERE produced_by_operation_journal_id=?1 ORDER BY journal_id", options)
	case sharedSelectSqliteMasterc39e:
		return sqlitex.Execute(conn, "SELECT ?3 FROM sqlite_master WHERE type=?1 AND name=?2", appendStaticSQLArgs(options, 1))
	case sharedUpdateTasksf343:
		return sqlitex.Execute(conn, "UPDATE tasks SET last_journal_id = ?1 WHERE id = ?2", options)
	default:
		return unknownSQLStatementError("sharedStatement", uint16(statement))
	}
}
