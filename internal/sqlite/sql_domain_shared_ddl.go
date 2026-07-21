package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type sharedDDL uint16

const (
	sharedDDLBeginStatement4e51 sharedDDL = iota + 1
	sharedDDLCommitStatement696a
	sharedDDLCreateIdxJournalActor886e
	sharedDDLCreateIdxJournalKind8f0c
	sharedDDLCreateIdxJournalRecordedAtaa92
	sharedDDLCreateIdxTasksNamespace7486
	sharedDDLCreateJournal4045
	sharedDDLDropJournal87b7
	sharedDDLDropJournalAttributed95ee
	sharedDDLDropJournalOperations8369
	sharedDDLDropJournalOperationsCanonicalInsertb583
	sharedDDLDropJournalOperationsCanonicalUpdate213c
	sharedDDLPragmaForeignKeyCheck6847
	sharedDDLPragmaForeignKeys1be4
	sharedDDLPragmaForeignKeysde7c
	sharedDDLRollbackStatement4eec
)

func (sharedDDL) statementClass() sqlStatementClass { return sqlDDLStatement }

func (statement sharedDDL) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case sharedDDLBeginStatement4e51:
		return sqlitex.ExecuteTransient(conn, "BEGIN IMMEDIATE", options)
	case sharedDDLCommitStatement696a:
		return sqlitex.ExecuteTransient(conn, "COMMIT", options)
	case sharedDDLCreateIdxJournalActor886e:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_journal_actor ON journal (actor_id)", options)
	case sharedDDLCreateIdxJournalKind8f0c:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_journal_kind  ON journal (kind_id)", options)
	case sharedDDLCreateIdxJournalRecordedAtaa92:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_journal_recorded_at ON journal (recorded_at, journal_id)", options)
	case sharedDDLCreateIdxTasksNamespace7486:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_namespace ON tasks (namespace)", options)
	case sharedDDLCreateJournal4045:
		return sqlitex.ExecuteTransient(conn, "CREATE VIEW IF NOT EXISTS journal_attributed AS SELECT j.journal_id AS journal_id,j.kind_id AS kind_id,COALESCE(j.actor_id,anchor.actor_id) AS effective_actor_id,j.recorded_at AS recorded_at,j.produced_by_operation_journal_id AS produced_by_operation_journal_id FROM journal j LEFT JOIN journal anchor ON anchor.journal_id=j.produced_by_operation_journal_id", options)
	case sharedDDLDropJournal87b7:
		return sqlitex.ExecuteTransient(conn, "DROP TABLE journal", options)
	case sharedDDLDropJournalAttributed95ee:
		return sqlitex.ExecuteTransient(conn, "DROP VIEW IF EXISTS journal_attributed", options)
	case sharedDDLDropJournalOperations8369:
		return sqlitex.ExecuteTransient(conn, "DROP TABLE journal_operations", options)
	case sharedDDLDropJournalOperationsCanonicalInsertb583:
		return sqlitex.ExecuteTransient(conn, "DROP TRIGGER IF EXISTS journal_operations_canonical_insert", options)
	case sharedDDLDropJournalOperationsCanonicalUpdate213c:
		return sqlitex.ExecuteTransient(conn, "DROP TRIGGER IF EXISTS journal_operations_canonical_update", options)
	case sharedDDLPragmaForeignKeyCheck6847:
		return sqlitex.ExecuteTransient(conn, "PRAGMA foreign_key_check", options)
	case sharedDDLPragmaForeignKeys1be4:
		return sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys=OFF", options)
	case sharedDDLPragmaForeignKeysde7c:
		return sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys=ON", options)
	case sharedDDLRollbackStatement4eec:
		return sqlitex.ExecuteTransient(conn, "ROLLBACK", options)
	default:
		return unknownSQLStatementError("sharedDDL", uint16(statement))
	}
}
