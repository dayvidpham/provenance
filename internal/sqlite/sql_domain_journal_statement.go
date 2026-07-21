package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type journalStatement uint16

const (
	journalInsertJournal11c0 journalStatement = iota + 1
	journalInsertJournalKinds9c1d
	journalInsertJournalTaskEventContextsc7c0
	journalInsertJournalTaskEventsa34e
	journalInsertTaskAttributions5227
	journalPragmaForeignKeyCheck6847
	journalSelectJournal3447
	journalSelectJournal4c92
	journalSelectJournal8eaf
	journalSelectJournal91db
	journalSelectJournalAttributed1222
	journalSelectJournalAttributedfe94
	journalSelectJournalAuthorities27f5
	journalSelectJournalAuthorities312f
	journalSelectJournalAuthorities4bc3
	journalSelectJournalAuthorities52fe
	journalSelectJournalAuthoritiesce9f
	journalSelectJournalAuthorityAssignmentEpisodes0701
	journalSelectJournalAuthorityAssignmentTransitionsfda0
	journalSelectJournalAuthorityBootstraps5c45
	journalSelectJournalDecisions1de4
	journalSelectJournalDecisions9b8c
	journalSelectJournalEvidence9ca1
	journalSelectJournalOperations37bc
	journalSelectJournalOperations5200
	journalSelectJournalOperations9eb6
	journalSelectJournalOperationse306
	journalSelectJournalOperationsfec3
	journalSelectJournalTaskEventContexts4e72
	journalSelectJournalTaskEvents1c74
	journalSelectJournalTaskEventsb7ba
	journalSelectJournalTaskEventsc599
	journalSelectJournalTaskEventsea01
	journalSelectJournalb993
	journalSelectJournale4f8
	journalSelectJournale6d7
	journalSelectTaskAttributions295b
	journalSelectTasks6f20
)

func (journalStatement) statementClass() sqlStatementClass { return sqlDMLStatement }

func (statement journalStatement) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case journalInsertJournal11c0:
		return sqlitex.Execute(conn, "INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id)\n\t\t VALUES (?1, ?2, ?3, ?4)", appendStaticSQLArgs(options, nil))
	case journalInsertJournalKinds9c1d:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO journal_kinds (id, name) VALUES (?1, ?2)", options)
	case journalInsertJournalTaskEventContextsc7c0:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO journal_task_event_contexts\n\t\t\t\t(event_journal_id, context_kind, context_identity, attached_by_journal_id)\n\t\t\t VALUES (?1, ?2, ?3, ?4)", options)
	case journalInsertJournalTaskEventsa34e:
		return sqlitex.Execute(conn, "INSERT INTO journal_task_events (journal_id, task_id, event_kind, payload)\n\t\t VALUES (?1, ?2, ?3, ?4)", options)
	case journalInsertTaskAttributions5227:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO task_attributions (task_id, actor_id, first_journal_id)\n\t\t VALUES (?1, ?2, ?3)", options)
	case journalPragmaForeignKeyCheck6847:
		return sqlitex.Execute(conn, "PRAGMA foreign_key_check", options)
	case journalSelectJournal3447:
		return sqlitex.Execute(conn, "SELECT j.journal_id FROM journal j LEFT JOIN journal_authorities s ON s.journal_id=j.journal_id WHERE j.kind_id=?1 AND s.journal_id IS ?2 LIMIT ?3", appendStaticSQLArgs(options, nil, 1))
	case journalSelectJournal4c92:
		return sqlitex.Execute(conn, "SELECT journal_id, produced_by_operation_journal_id IS NOT ?1\n\t\t FROM journal\n\t\t WHERE (produced_by_operation_journal_id IS NOT ?2 AND actor_id IS NOT ?3)\n\t\t    OR (produced_by_operation_journal_id IS ?4     AND actor_id IS ?5)\n\t\t LIMIT ?6", appendStaticSQLArgs(options, nil, nil, nil, nil, nil, 1))
	case journalSelectJournal8eaf:
		return sqlitex.Execute(conn, "SELECT j.journal_id FROM journal j LEFT JOIN journal_decisions s ON s.journal_id=j.journal_id WHERE j.kind_id=?1 AND s.journal_id IS ?2 LIMIT ?3", appendStaticSQLArgs(options, nil, 1))
	case journalSelectJournal91db:
		return sqlitex.Execute(conn, "SELECT j.journal_id FROM journal j LEFT JOIN journal_operations s ON s.journal_id=j.journal_id WHERE j.kind_id=?1 AND s.journal_id IS ?2 LIMIT ?3", appendStaticSQLArgs(options, nil, 1))
	case journalSelectJournalAttributed1222:
		return sqlitex.Execute(conn, "SELECT j.journal_id,j.effective_actor_id,j.recorded_at,te.task_id,te.event_kind,te.payload\n\t\t\tFROM journal_attributed j JOIN journal_task_events te ON te.journal_id=j.journal_id\n\t\t\tWHERE j.journal_id<=?1 AND j.journal_id>?3\n\t\t\t  AND (NOT ?4 OR te.task_id IN (SELECT value FROM json_each(?5)))\n\t\t\t  AND (NOT ?6 OR te.event_kind IN (SELECT value FROM json_each(?7)))\n\t\t\t  AND (NOT ?8 OR EXISTS (SELECT ?13 FROM journal_task_event_contexts ctx JOIN json_each(?9) f ON ctx.context_kind=json_extract(f.value,?10) AND ctx.context_identity=json_extract(f.value,?11) WHERE ctx.event_journal_id=te.journal_id))\n\t\t\tORDER BY j.journal_id ASC LIMIT ?12", appendStaticSQLArgs(options, 1))
	case journalSelectJournalAttributedfe94:
		return sqlitex.Execute(conn, "SELECT j.journal_id,j.effective_actor_id,j.recorded_at,te.task_id,te.event_kind,te.payload\n\t\t\tFROM journal_attributed j JOIN journal_task_events te ON te.journal_id=j.journal_id\n\t\t\tWHERE j.journal_id<=?1 AND (j.recorded_at>?2 OR (j.recorded_at=?2 AND j.journal_id>?3))\n\t\t\t  AND (NOT ?4 OR te.task_id IN (SELECT value FROM json_each(?5)))\n\t\t\t  AND (NOT ?6 OR te.event_kind IN (SELECT value FROM json_each(?7)))\n\t\t\t  AND (NOT ?8 OR EXISTS (SELECT ?13 FROM journal_task_event_contexts ctx JOIN json_each(?9) f ON ctx.context_kind=json_extract(f.value,?10) AND ctx.context_identity=json_extract(f.value,?11) WHERE ctx.event_journal_id=te.journal_id))\n\t\t\tORDER BY j.recorded_at ASC,j.journal_id ASC LIMIT ?12", appendStaticSQLArgs(options, 1))
	case journalSelectJournalAuthorities27f5:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_authorities a JOIN journal_evidence b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case journalSelectJournalAuthorities312f:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_authorities a\n\t\t\tLEFT JOIN journal_authority_assignment_transitions d ON d.journal_id = a.journal_id\n\t\t\tWHERE a.authority_kind_id = ?1 AND d.journal_id IS ?2 LIMIT ?3", appendStaticSQLArgs(options, nil, 1))
	case journalSelectJournalAuthorities4bc3:
		return sqlitex.Execute(conn, "SELECT s.journal_id FROM journal_authorities s JOIN journal j ON j.journal_id=s.journal_id WHERE j.kind_id<>?1 LIMIT ?2", appendStaticSQLArgs(options, 1))
	case journalSelectJournalAuthorities52fe:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_authorities a JOIN journal_decisions b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case journalSelectJournalAuthoritiesce9f:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_authorities a\n\t\t\tLEFT JOIN journal_authority_bootstraps d ON d.journal_id = a.journal_id\n\t\t\tWHERE a.authority_kind_id = ?1 AND d.journal_id IS ?2 LIMIT ?3", appendStaticSQLArgs(options, nil, 1))
	case journalSelectJournalAuthorityAssignmentEpisodes0701:
		return sqlitex.Execute(conn, "SELECT e.assignment_id FROM journal_authority_assignment_episodes e LEFT JOIN journal_authority_assignment_transitions t ON t.assignment_id=e.assignment_id WHERE t.journal_id IS ?1 LIMIT ?2", appendStaticSQLArgs(options, nil, 1))
	case journalSelectJournalAuthorityAssignmentTransitionsfda0:
		return sqlitex.Execute(conn, "SELECT d.journal_id FROM journal_authority_assignment_transitions d\n\t\t\tJOIN journal_authorities a ON a.journal_id = d.journal_id\n\t\t\tWHERE a.authority_kind_id <> ?1 LIMIT ?2", appendStaticSQLArgs(options, 1))
	case journalSelectJournalAuthorityBootstraps5c45:
		return sqlitex.Execute(conn, "SELECT d.journal_id FROM journal_authority_bootstraps d\n\t\t\tJOIN journal_authorities a ON a.journal_id = d.journal_id\n\t\t\tWHERE a.authority_kind_id <> ?1 LIMIT ?2", appendStaticSQLArgs(options, 1))
	case journalSelectJournalDecisions1de4:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_decisions a JOIN journal_evidence b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case journalSelectJournalDecisions9b8c:
		return sqlitex.Execute(conn, "SELECT s.journal_id FROM journal_decisions s JOIN journal j ON j.journal_id=s.journal_id WHERE j.kind_id<>?1 LIMIT ?2", appendStaticSQLArgs(options, 1))
	case journalSelectJournalEvidence9ca1:
		return sqlitex.Execute(conn, "SELECT s.journal_id FROM journal_evidence s JOIN journal j ON j.journal_id=s.journal_id WHERE j.kind_id<>?1 LIMIT ?2", appendStaticSQLArgs(options, 1))
	case journalSelectJournalOperations37bc:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_operations a JOIN journal_evidence b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case journalSelectJournalOperations5200:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_operations a JOIN journal_decisions b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case journalSelectJournalOperations9eb6:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_operations a JOIN journal_authorities b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case journalSelectJournalOperationse306:
		return sqlitex.Execute(conn, "SELECT s.journal_id FROM journal_operations s JOIN journal j ON j.journal_id=s.journal_id WHERE j.kind_id<>?1 LIMIT ?2", appendStaticSQLArgs(options, 1))
	case journalSelectJournalOperationsfec3:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_operations a JOIN journal_task_events b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case journalSelectJournalTaskEventContexts4e72:
		return sqlitex.Execute(conn, "SELECT context_kind, context_identity FROM journal_task_event_contexts\n\t\t WHERE event_journal_id = ?1 ORDER BY context_kind, context_identity", options)
	case journalSelectJournalTaskEvents1c74:
		return sqlitex.Execute(conn, "SELECT s.journal_id FROM journal_task_events s JOIN journal j ON j.journal_id=s.journal_id WHERE j.kind_id<>?1 LIMIT ?2", appendStaticSQLArgs(options, 1))
	case journalSelectJournalTaskEventsb7ba:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_task_events a JOIN journal_authorities b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case journalSelectJournalTaskEventsc599:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_task_events a JOIN journal_decisions b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case journalSelectJournalTaskEventsea01:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_task_events a JOIN journal_evidence b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case journalSelectJournalb993:
		return sqlitex.Execute(conn, "SELECT j.journal_id FROM journal j LEFT JOIN journal_task_events s ON s.journal_id=j.journal_id WHERE j.kind_id=?1 AND s.journal_id IS ?2 LIMIT ?3", appendStaticSQLArgs(options, nil, 1))
	case journalSelectJournale4f8:
		return sqlitex.Execute(conn, "SELECT j.journal_id FROM journal j LEFT JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id WHERE j.produced_by_operation_journal_id IS NOT ?1 AND o.journal_id IS ?2 LIMIT ?3", appendStaticSQLArgs(options, nil, nil, 1))
	case journalSelectJournale6d7:
		return sqlitex.Execute(conn, "SELECT j.journal_id FROM journal j LEFT JOIN journal_evidence s ON s.journal_id=j.journal_id WHERE j.kind_id=?1 AND s.journal_id IS ?2 LIMIT ?3", appendStaticSQLArgs(options, nil, 1))
	case journalSelectTaskAttributions295b:
		return sqlitex.Execute(conn, "SELECT task_id, actor_id, first_journal_id FROM task_attributions\n\t\t WHERE task_id = ?1 ORDER BY first_journal_id ASC", options)
	case journalSelectTasks6f20:
		return sqlitex.Execute(conn, "SELECT id FROM tasks WHERE last_journal_id IS ?1 LIMIT ?2", appendStaticSQLArgs(options, nil, 1))
	default:
		return unknownSQLStatementError("journalStatement", uint16(statement))
	}
}
