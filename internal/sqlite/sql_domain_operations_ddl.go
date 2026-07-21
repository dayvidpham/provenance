package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type operationsDDL uint16

const (
	operationsDDLAlterJournalNewe065 operationsDDL = iota + 1
	operationsDDLAlterJournalOperationsGeneric12a2
	operationsDDLAlterJournalOperationsd9aa
	operationsDDLAlterJournalOperationsf8a9
	operationsDDLCreateAssignmentSlots0011
	operationsDDLCreateAssignmentTransitions6d60
	operationsDDLCreateAuthorityKinds7fea
	operationsDDLCreateIdxEpisodesParent6a12
	operationsDDLCreateIdxEpisodesTask82f2
	operationsDDLCreateIdxJournalPboj4aa6
	operationsDDLCreateIdxTransitionsAssignment578e
	operationsDDLCreateJournalAuthorities340f
	operationsDDLCreateJournalAuthorityAssignmentTransitionsc955
	operationsDDLCreateJournalAuthorityBootstraps8089
	operationsDDLCreateJournalDecisions8099
	operationsDDLCreateJournalEvidencef3d8
	operationsDDLCreateJournalNew805a
	operationsDDLCreateJournalOperationResultSlots4238
	operationsDDLCreateJournalOperations64cc
	operationsDDLCreateJournalOperationsCanonicalInsert26d0
	operationsDDLCreateJournalOperationsCanonicalInserteb25
	operationsDDLCreateJournalOperationsGenericad97
	operationsDDLCreateOf3f0e
	operationsDDLCreateOfd2e8
	operationsDDLCreateThe911e
	operationsDDLPragmaForeignKeyListf38a
	operationsDDLPragmaTableInfoad7d
)

func (operationsDDL) statementClass() sqlStatementClass { return sqlDDLStatement }

func (statement operationsDDL) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case operationsDDLAlterJournalNewe065:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE journal_new RENAME TO journal", options)
	case operationsDDLAlterJournalOperationsGeneric12a2:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE journal_operations_generic RENAME TO journal_operations", options)
	case operationsDDLAlterJournalOperationsd9aa:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE journal_operations ADD COLUMN canonical_mutation BLOB", options)
	case operationsDDLAlterJournalOperationsf8a9:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE journal_operations ADD COLUMN mutation_encoding_version TEXT", options)
	case operationsDDLCreateAssignmentSlots0011:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS assignment_slots (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case operationsDDLCreateAssignmentTransitions6d60:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS assignment_transitions (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case operationsDDLCreateAuthorityKinds7fea:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS authority_kinds (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case operationsDDLCreateIdxEpisodesParent6a12:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_episodes_parent ON journal_authority_assignment_episodes (parent_assignment_id)", options)
	case operationsDDLCreateIdxEpisodesTask82f2:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_episodes_task ON journal_authority_assignment_episodes (task_id)", options)
	case operationsDDLCreateIdxJournalPboj4aa6:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_journal_pboj  ON journal (produced_by_operation_journal_id)", options)
	case operationsDDLCreateIdxTransitionsAssignment578e:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_transitions_assignment ON journal_authority_assignment_transitions (assignment_id)", options)
	case operationsDDLCreateJournalAuthorities340f:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_authorities (\n\t\t\tjournal_id              INTEGER PRIMARY KEY REFERENCES journal(journal_id),\n\t\t\tauthority_kind_id      INTEGER NOT NULL REFERENCES authority_kinds(id),\n\t\t\toperation_authority_id TEXT NOT NULL UNIQUE\n\t\t) STRICT", options)
	case operationsDDLCreateJournalAuthorityAssignmentTransitionsc955:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_authority_assignment_transitions (\n\t\t\tjournal_id     INTEGER PRIMARY KEY REFERENCES journal_authorities(journal_id),\n\t\t\tassignment_id TEXT NOT NULL REFERENCES journal_authority_assignment_episodes(assignment_id),\n\t\t\ttransition_id INTEGER NOT NULL REFERENCES assignment_transitions(id),\n\t\t\tUNIQUE (assignment_id, transition_id)\n\t\t) STRICT", options)
	case operationsDDLCreateJournalAuthorityBootstraps8089:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_authority_bootstraps (\n\t\t\tjournal_id INTEGER PRIMARY KEY REFERENCES journal_authorities(journal_id),\n\t\t\tlabel     TEXT NOT NULL\n\t\t) STRICT", options)
	case operationsDDLCreateJournalDecisions8099:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_decisions (\n\t\t\tjournal_id     INTEGER PRIMARY KEY REFERENCES journal(journal_id),\n\t\t\tdecision_kind TEXT NOT NULL,\n\t\t\ttask_id       TEXT REFERENCES tasks(id),\n\t\t\tpayload       TEXT NOT NULL CHECK (json_valid(payload))\n\t\t) STRICT", options)
	case operationsDDLCreateJournalEvidencef3d8:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_evidence (\n\t\t\tjournal_id      INTEGER PRIMARY KEY REFERENCES journal(journal_id),\n\t\t\tevidence_kind  TEXT NOT NULL,\n\t\t\ttask_id        TEXT REFERENCES tasks(id),\n\t\t\tcontent_digest BLOB NOT NULL,\n\t\t\tpayload        TEXT NOT NULL CHECK (json_valid(payload))\n\t\t) STRICT", options)
	case operationsDDLCreateJournalNew805a:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE journal_new (\n\t\t\tjournal_id   INTEGER PRIMARY KEY AUTOINCREMENT,\n\t\t\tkind_id     INTEGER NOT NULL REFERENCES journal_kinds(id),\n\t\t\tactor_id    TEXT REFERENCES agents(id),\n\t\t\trecorded_at INTEGER NOT NULL,\n\t\t\tproduced_by_operation_journal_id INTEGER REFERENCES journal_operations(journal_id),\n\t\t\tCHECK ((actor_id IS NULL) = (produced_by_operation_journal_id IS NOT NULL)),\n\t\t\tCHECK (kind_id <> 1 OR produced_by_operation_journal_id IS NOT NULL)\n\t\t) STRICT", options)
	case operationsDDLCreateJournalOperationResultSlots4238:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_operation_result_slots (\n\t\t\tjournal_id           INTEGER NOT NULL REFERENCES journal_operations(journal_id),\n\t\t\tresult_slot_id      TEXT NOT NULL,\n\t\t\tproduced_journal_id INTEGER NOT NULL REFERENCES journal(journal_id),\n\t\t\tPRIMARY KEY (journal_id, result_slot_id)\n\t\t) STRICT, WITHOUT ROWID", options)
	case operationsDDLCreateJournalOperations64cc:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_operations (\n\t\t\tjournal_id            INTEGER PRIMARY KEY REFERENCES journal(journal_id),\n\t\t\toperation_id         TEXT NOT NULL UNIQUE,\n\t\t\tauthority_journal_id INTEGER REFERENCES journal_authorities(journal_id),\n\t\t\tcommand_digest       BLOB NOT NULL,\n\t\t\tmutation_digest      BLOB NOT NULL,\n\t\t\tmutation_encoding_version TEXT,\n\t\t\tcanonical_mutation   BLOB,\n\t\t\tCHECK ((mutation_encoding_version IS NULL AND canonical_mutation IS NULL) OR\n\t\t\t       (length(mutation_encoding_version) > 0 AND length(canonical_mutation) > 0))\n\t\t) STRICT", options)
	case operationsDDLCreateJournalOperationsCanonicalInsert26d0:
		return sqlitex.ExecuteTransient(conn, "CREATE TRIGGER IF NOT EXISTS journal_operations_canonical_insert\n\t\t BEFORE INSERT ON journal_operations\n\t\t WHEN NOT ((NEW.mutation_encoding_version IS NULL AND NEW.canonical_mutation IS NULL) OR\n\t\t\t           (length(NEW.mutation_encoding_version) > 0 AND length(NEW.canonical_mutation) > 0))\n\t\t BEGIN SELECT RAISE(ABORT, 'invalid canonical mutation version/bytes pair'); END", options)
	case operationsDDLCreateJournalOperationsCanonicalInserteb25:
		return sqlitex.ExecuteTransient(conn, "CREATE TRIGGER journal_operations_canonical_insert BEFORE INSERT ON journal_operations WHEN NOT ((NEW.mutation_encoding_version IS NULL AND NEW.canonical_mutation IS NULL) OR (length(NEW.mutation_encoding_version)>0 AND length(NEW.canonical_mutation)>0)) BEGIN SELECT RAISE(ABORT,'invalid canonical mutation version/bytes pair'); END", options)
	case operationsDDLCreateJournalOperationsGenericad97:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE journal_operations_generic (journal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id),operation_id TEXT NOT NULL UNIQUE,authority_journal_id INTEGER REFERENCES journal_authorities(journal_id),command_digest BLOB NOT NULL,mutation_digest BLOB NOT NULL,mutation_encoding_version TEXT,canonical_mutation BLOB,CHECK ((mutation_encoding_version IS NULL AND canonical_mutation IS NULL) OR (length(mutation_encoding_version)>0 AND length(canonical_mutation)>0))) STRICT", options)
	case operationsDDLCreateOf3f0e:
		return sqlitex.ExecuteTransient(conn, "CREATE TRIGGER IF NOT EXISTS journal_operations_canonical_update\n\t\t BEFORE UPDATE OF mutation_encoding_version, canonical_mutation ON journal_operations\n\t\t WHEN NOT ((NEW.mutation_encoding_version IS NULL AND NEW.canonical_mutation IS NULL) OR\n\t\t\t           (length(NEW.mutation_encoding_version) > 0 AND length(NEW.canonical_mutation) > 0))\n\t\t BEGIN SELECT RAISE(ABORT, 'invalid canonical mutation version/bytes pair'); END", options)
	case operationsDDLCreateOfd2e8:
		return sqlitex.ExecuteTransient(conn, "CREATE TRIGGER journal_operations_canonical_update BEFORE UPDATE OF mutation_encoding_version,canonical_mutation ON journal_operations WHEN NOT ((NEW.mutation_encoding_version IS NULL AND NEW.canonical_mutation IS NULL) OR (length(NEW.mutation_encoding_version)>0 AND length(NEW.canonical_mutation)>0)) BEGIN SELECT RAISE(ABORT,'invalid canonical mutation version/bytes pair'); END", options)
	case operationsDDLCreateThe911e:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_authority_assignment_episodes (\n\t\t\tassignment_id             TEXT PRIMARY KEY,\n\t\t\ttask_id                   TEXT NOT NULL REFERENCES tasks(id),\n\t\t\tslot_id                   INTEGER NOT NULL REFERENCES assignment_slots(id),\n\t\t\tactor_id                  TEXT NOT NULL REFERENCES agents(id),\n\t\t\tpredecessor_assignment_id TEXT UNIQUE REFERENCES journal_authority_assignment_episodes(assignment_id),\n\t\t\t-- ParentAssignmentID (§14.5): deliberate governance-citation edge, cited at\n\t\t\t-- start; NOT UNIQUE (one parent may govern many children), distinct from the\n\t\t\t-- UNIQUE predecessor (succession) edge above.\n\t\t\tparent_assignment_id      TEXT REFERENCES journal_authority_assignment_episodes(assignment_id)\n\t\t) STRICT", options)
	case operationsDDLPragmaForeignKeyListf38a:
		return sqlitex.ExecuteTransient(conn, "PRAGMA foreign_key_list(journal)", options)
	case operationsDDLPragmaTableInfoad7d:
		return sqlitex.ExecuteTransient(conn, "PRAGMA table_info(journal_operations)", options)
	default:
		return unknownSQLStatementError("operationsDDL", uint16(statement))
	}
}
