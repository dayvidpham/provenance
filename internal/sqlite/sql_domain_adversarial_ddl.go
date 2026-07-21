package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type adversarialDDL uint16

const (
	adversarialDDLAlterJournalLegacya8de adversarialDDL = iota + 1
	adversarialDDLAlterJournalOperationsV15b33
	adversarialDDLAlterJournalTaskEvents4d3f
	adversarialDDLAlterJournalTaskEvents8346
	adversarialDDLCreateIdxJournalActora6ef
	adversarialDDLCreateIdxJournalKind725e
	adversarialDDLCreateIdxJournalPboj391f
	adversarialDDLCreateIdxJournalRecordedAt69e6
	adversarialDDLCreateJournalLegacy3d98
	adversarialDDLCreateJournalOperationsCanonicalInsertbb5e
	adversarialDDLCreateJournalOperationsV13987
	adversarialDDLCreateJournalUnreviewed8b03
	adversarialDDLCreateOff2d8
	adversarialDDLPragmaIgnoreCheckConstraints8d56
	adversarialDDLPragmaIgnoreCheckConstraintsb381
)

func (adversarialDDL) statementClass() sqlStatementClass { return sqlDDLStatement }

func (statement adversarialDDL) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case adversarialDDLAlterJournalLegacya8de:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE journal_legacy RENAME TO journal", options)
	case adversarialDDLAlterJournalOperationsV15b33:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE journal_operations_v1 RENAME TO journal_operations", options)
	case adversarialDDLAlterJournalTaskEvents4d3f:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE journal_task_events ADD COLUMN unreviewed TEXT", options)
	case adversarialDDLAlterJournalTaskEvents8346:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE journal_task_events DROP COLUMN payload", options)
	case adversarialDDLCreateIdxJournalActora6ef:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX idx_journal_actor ON journal(actor_id)", options)
	case adversarialDDLCreateIdxJournalKind725e:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX idx_journal_kind ON journal(kind_id)", options)
	case adversarialDDLCreateIdxJournalPboj391f:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX idx_journal_pboj ON journal(produced_by_operation_journal_id)", options)
	case adversarialDDLCreateIdxJournalRecordedAt69e6:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX idx_journal_recorded_at ON journal(recorded_at,journal_id)", options)
	case adversarialDDLCreateJournalLegacy3d98:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE journal_legacy (journal_id INTEGER PRIMARY KEY AUTOINCREMENT,kind_id INTEGER NOT NULL REFERENCES journal_kinds(id),actor_id TEXT REFERENCES agents(id),recorded_at INTEGER NOT NULL,produced_by_operation_journal_id INTEGER,CHECK ((actor_id IS NULL) = (produced_by_operation_journal_id IS NOT NULL)),CHECK (kind_id <> 1 OR produced_by_operation_journal_id IS NOT NULL)) STRICT", options)
	case adversarialDDLCreateJournalOperationsCanonicalInsertbb5e:
		return sqlitex.ExecuteTransient(conn, "CREATE TRIGGER journal_operations_canonical_insert BEFORE INSERT ON journal_operations WHEN NEW.mutation_encoding_version IS NOT NULL AND NEW.mutation_encoding_version!='provenance.mutation.v1' BEGIN SELECT RAISE(ABORT,'V1 only'); END", options)
	case adversarialDDLCreateJournalOperationsV13987:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE journal_operations_v1 (journal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id),operation_id TEXT NOT NULL UNIQUE,authority_journal_id INTEGER REFERENCES journal_authorities(journal_id),command_digest BLOB NOT NULL,mutation_digest BLOB NOT NULL,mutation_encoding_version TEXT,canonical_mutation BLOB,CHECK ((mutation_encoding_version IS NULL AND canonical_mutation IS NULL) OR (mutation_encoding_version='provenance.mutation.v1' AND length(canonical_mutation)>0))) STRICT", options)
	case adversarialDDLCreateJournalUnreviewed8b03:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE journal_unreviewed (journal_id INTEGER PRIMARY KEY) STRICT", options)
	case adversarialDDLCreateOff2d8:
		return sqlitex.ExecuteTransient(conn, "CREATE TRIGGER journal_operations_canonical_update BEFORE UPDATE OF mutation_encoding_version ON journal_operations WHEN NEW.mutation_encoding_version IS NOT NULL AND NEW.mutation_encoding_version!='provenance.mutation.v1' BEGIN SELECT RAISE(ABORT,'V1 only'); END", options)
	case adversarialDDLPragmaIgnoreCheckConstraints8d56:
		return sqlitex.ExecuteTransient(conn, "PRAGMA ignore_check_constraints=ON", options)
	case adversarialDDLPragmaIgnoreCheckConstraintsb381:
		return sqlitex.ExecuteTransient(conn, "PRAGMA ignore_check_constraints=OFF", options)
	default:
		return unknownSQLStatementError("adversarialDDL", uint16(statement))
	}
}
