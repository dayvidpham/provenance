package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type journalDDL uint16

const (
	journalDDLCreateActorNamespaceClaims9907 journalDDL = iota + 1
	journalDDLCreateFixedActorManifestEntries6754
	journalDDLCreateIdxJournalTaskEventsTask77b9
	journalDDLCreateJournalKinds82d6
	journalDDLCreateJournalTaskEventContexts4467
	journalDDLCreateJournalTaskEvents237d
	journalDDLCreateJournald6cb
	journalDDLCreateJournaleedf
	journalDDLCreateTaskAttributions2a9e
)

func (journalDDL) statementClass() sqlStatementClass { return sqlDDLStatement }

func (statement journalDDL) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case journalDDLCreateActorNamespaceClaims9907:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS actor_namespace_claims (\n\t\t\tnamespace   TEXT PRIMARY KEY,\n\t\t\tclaimant_id TEXT NOT NULL,\n\t\t\trange_min   BLOB NOT NULL,\n\t\t\trange_max   BLOB NOT NULL,\n\t\t\tcodec       TEXT NOT NULL\n\t\t) STRICT", options)
	case journalDDLCreateFixedActorManifestEntries6754:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS fixed_actor_manifest_entries (\n\t\t\tactor_id  TEXT PRIMARY KEY REFERENCES agents(id),\n\t\t\tnamespace TEXT NOT NULL REFERENCES actor_namespace_claims(namespace),\n\t\t\tkind_id   INTEGER NOT NULL REFERENCES agent_kinds(id),\n\t\t\tname      TEXT NOT NULL,\n\t\t\tmetadata  TEXT NOT NULL CHECK (json_valid(metadata)),\n\t\t\tUNIQUE (namespace, name)\n\t\t) STRICT", options)
	case journalDDLCreateIdxJournalTaskEventsTask77b9:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_journal_task_events_task ON journal_task_events (task_id)", options)
	case journalDDLCreateJournalKinds82d6:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_kinds (\n\t\t\tid   INTEGER PRIMARY KEY,\n\t\t\tname TEXT NOT NULL UNIQUE\n\t\t) STRICT", options)
	case journalDDLCreateJournalTaskEventContexts4467:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_task_event_contexts (\n\t\t\tevent_journal_id       INTEGER NOT NULL REFERENCES journal_task_events(journal_id),\n\t\t\tcontext_kind           TEXT NOT NULL,\n\t\t\tcontext_identity       TEXT NOT NULL,\n\t\t\tattached_by_journal_id INTEGER NOT NULL REFERENCES journal_task_events(journal_id),\n\t\t\tPRIMARY KEY (event_journal_id, context_kind, context_identity)\n\t\t) STRICT, WITHOUT ROWID", options)
	case journalDDLCreateJournalTaskEvents237d:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_task_events (\n\t\t\tjournal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id),\n\t\t\ttask_id    TEXT NOT NULL REFERENCES tasks(id),\n\t\t\tevent_kind TEXT NOT NULL,\n\t\t\tpayload    TEXT NOT NULL CHECK (json_valid(payload))\n\t\t) STRICT", options)
	case journalDDLCreateJournald6cb:
		return sqlitex.ExecuteTransient(conn, "CREATE VIEW IF NOT EXISTS journal_attributed AS\n\t\t SELECT j.journal_id AS journal_id,j.kind_id AS kind_id,COALESCE(j.actor_id,anchor.actor_id) AS effective_actor_id,\n\t\t j.recorded_at AS recorded_at,j.produced_by_operation_journal_id AS produced_by_operation_journal_id\n\t\t FROM journal j LEFT JOIN journal anchor ON anchor.journal_id=j.produced_by_operation_journal_id", options)
	case journalDDLCreateJournaleedf:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal (\n\t\t\tjournal_id  INTEGER PRIMARY KEY AUTOINCREMENT,\n\t\t\tkind_id     INTEGER NOT NULL REFERENCES journal_kinds(id),\n\t\t\tactor_id    TEXT REFERENCES agents(id),\n\t\t\trecorded_at INTEGER NOT NULL,\n\t\t\t-- The producing operation (§2.1, §4.6). NULL at the journal-base layer;\n\t\t\t-- the operations slice (dayvidpham/provenance#5) adds the FK to\n\t\t\t-- journal_operations(journal_id) when that subtype table lands.\n\t\t\tproduced_by_operation_journal_id INTEGER,\n\t\t\tCHECK ((actor_id IS NULL) = (produced_by_operation_journal_id IS NOT NULL))\n\t\t) STRICT", options)
	case journalDDLCreateTaskAttributions2a9e:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS task_attributions (\n\t\t\ttask_id          TEXT NOT NULL REFERENCES tasks(id),\n\t\t\tactor_id         TEXT NOT NULL REFERENCES agents(id),\n\t\t\tfirst_journal_id INTEGER NOT NULL REFERENCES journal(journal_id),\n\t\t\tPRIMARY KEY (task_id, actor_id)\n\t\t) STRICT, WITHOUT ROWID", options)
	default:
		return unknownSQLStatementError("journalDDL", uint16(statement))
	}
}
