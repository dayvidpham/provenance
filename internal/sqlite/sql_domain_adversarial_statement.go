package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type adversarialStatement uint16

const (
	adversarialDeleteJournalAuthorities0a99 adversarialStatement = iota + 1
	adversarialDeleteJournalAuthoritiesc8ba
	adversarialDeleteJournalAuthorityAssignmentTransitions22ab
	adversarialDeleteJournalAuthorityBootstraps3ee3
	adversarialDeleteJournalDecisionsa790
	adversarialDeleteJournalDecisionsd136
	adversarialDeleteJournalEvidence4f20
	adversarialDeleteJournalEvidence6f75
	adversarialDeleteJournalOperationResultSlots436c
	adversarialDeleteJournalOperations07ab
	adversarialDeleteJournalOperations1c0e
	adversarialDeleteJournalTaskEventContexts7d1d
	adversarialDeleteJournalTaskEvents1040
	adversarialDeleteJournalTaskEventsee69
	adversarialDeleteJournalb965
	adversarialInsertJournalAuthorityAssignmentEpisodes2c6e
	adversarialInsertJournalAuthorityAssignmentEpisodesd5d6
	adversarialInsertJournalDecisions7778
	adversarialInsertJournalEvidenceb639
	adversarialInsertJournalLegacyd55f
	adversarialInsertJournalOperationsV144cf
	adversarialInsertJournalOperationsd00f
	adversarialInsertJournald134
	adversarialInsertTaskAttributions0464
	adversarialSelectJournal3863
	adversarialSelectJournal4577
	adversarialSelectJournalb2c5
	adversarialSelectJournalc071
	adversarialUpdateComments6627
	adversarialUpdateJournal31e7
	adversarialUpdateJournalAuthorityAssignmentEpisodesf918
	adversarialUpdateTasks4c22
	adversarialUpdateTasks9d9f
	adversarialUpdateTasksc047
)

func (adversarialStatement) statementClass() sqlStatementClass { return sqlDMLStatement }

func (statement adversarialStatement) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case adversarialDeleteJournalAuthorities0a99:
		return sqlitex.Execute(conn, "DELETE FROM journal_authorities WHERE journal_id = ?1", options)
	case adversarialDeleteJournalAuthoritiesc8ba:
		return sqlitex.Execute(conn, "DELETE FROM journal_authorities WHERE journal_id=?1", options)
	case adversarialDeleteJournalAuthorityAssignmentTransitions22ab:
		return sqlitex.Execute(conn, "DELETE FROM journal_authority_assignment_transitions WHERE journal_id = ?1", options)
	case adversarialDeleteJournalAuthorityBootstraps3ee3:
		return sqlitex.Execute(conn, "DELETE FROM journal_authority_bootstraps WHERE journal_id = ?1", options)
	case adversarialDeleteJournalDecisionsa790:
		return sqlitex.Execute(conn, "DELETE FROM journal_decisions WHERE journal_id = ?1", options)
	case adversarialDeleteJournalDecisionsd136:
		return sqlitex.Execute(conn, "DELETE FROM journal_decisions WHERE journal_id=?1", options)
	case adversarialDeleteJournalEvidence4f20:
		return sqlitex.Execute(conn, "DELETE FROM journal_evidence WHERE journal_id=?1", options)
	case adversarialDeleteJournalEvidence6f75:
		return sqlitex.Execute(conn, "DELETE FROM journal_evidence WHERE journal_id = ?1", options)
	case adversarialDeleteJournalOperationResultSlots436c:
		return sqlitex.Execute(conn, "DELETE FROM journal_operation_result_slots WHERE journal_id = ?1", options)
	case adversarialDeleteJournalOperations07ab:
		return sqlitex.Execute(conn, "DELETE FROM journal_operations WHERE journal_id=?1", options)
	case adversarialDeleteJournalOperations1c0e:
		return sqlitex.Execute(conn, "DELETE FROM journal_operations WHERE journal_id = ?1", options)
	case adversarialDeleteJournalTaskEventContexts7d1d:
		return sqlitex.Execute(conn, "DELETE FROM journal_task_event_contexts WHERE event_journal_id = ?1", options)
	case adversarialDeleteJournalTaskEvents1040:
		return sqlitex.Execute(conn, "DELETE FROM journal_task_events WHERE journal_id = ?1", options)
	case adversarialDeleteJournalTaskEventsee69:
		return sqlitex.Execute(conn, "DELETE FROM journal_task_events WHERE journal_id=?1", options)
	case adversarialDeleteJournalb965:
		return sqlitex.Execute(conn, "DELETE FROM journal WHERE journal_id = ?1", options)
	case adversarialInsertJournalAuthorityAssignmentEpisodes2c6e:
		return sqlitex.Execute(conn, "INSERT INTO journal_authority_assignment_episodes (assignment_id, task_id, slot_id, actor_id, predecessor_assignment_id, parent_assignment_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?6, ?5)", appendStaticSQLArgs(options, nil))
	case adversarialInsertJournalAuthorityAssignmentEpisodesd5d6:
		return sqlitex.Execute(conn, "INSERT INTO journal_authority_assignment_episodes (assignment_id, task_id, slot_id, actor_id, predecessor_assignment_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5)", appendStaticSQLArgs(options, nil))
	case adversarialInsertJournalDecisions7778:
		return sqlitex.Execute(conn, "INSERT INTO journal_decisions (journal_id, decision_kind, task_id, payload) VALUES (?1, ?2, ?4, ?3)", appendStaticSQLArgs(options, nil))
	case adversarialInsertJournalEvidenceb639:
		return sqlitex.Execute(conn, "INSERT INTO journal_evidence (journal_id, evidence_kind, task_id, content_digest, payload) VALUES (?1, ?2, ?5, ?3, ?4)", appendStaticSQLArgs(options, nil))
	case adversarialInsertJournalLegacyd55f:
		return sqlitex.Execute(conn, "INSERT INTO journal_legacy SELECT * FROM journal", options)
	case adversarialInsertJournalOperationsV144cf:
		return sqlitex.Execute(conn, "INSERT INTO journal_operations_v1 SELECT * FROM journal_operations", options)
	case adversarialInsertJournalOperationsd00f:
		return sqlitex.Execute(conn, "INSERT INTO journal_operations (journal_id, operation_id, authority_journal_id, command_digest, mutation_digest)\n\t\t VALUES (?1, ?2, ?5, ?3, ?4)", appendStaticSQLArgs(options, nil))
	case adversarialInsertJournald134:
		return sqlitex.Execute(conn, "INSERT INTO journal (journal_id, kind_id, actor_id, recorded_at, produced_by_operation_journal_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5)", appendStaticSQLArgs(options, 0, nil))
	case adversarialInsertTaskAttributions0464:
		return sqlitex.Execute(conn, "INSERT OR REPLACE INTO task_attributions (task_id, actor_id, first_journal_id) VALUES (?1, ?2, ?3)", options)
	case adversarialSelectJournal3863:
		return sqlitex.Execute(conn, "SELECT kind_id FROM journal WHERE journal_id = ?1", options)
	case adversarialSelectJournal4577:
		return sqlitex.Execute(conn, "SELECT journal_id, kind_id FROM journal ORDER BY journal_id ASC", options)
	case adversarialSelectJournalb2c5:
		return sqlitex.Execute(conn, "SELECT journal_id FROM journal ORDER BY journal_id DESC LIMIT ?1", options)
	case adversarialSelectJournalc071:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal", options)
	case adversarialUpdateComments6627:
		return sqlitex.Execute(conn, "UPDATE comments SET body = ?1 WHERE id = ?2", options)
	case adversarialUpdateJournal31e7:
		return sqlitex.Execute(conn, "UPDATE journal SET kind_id = ?1 WHERE journal_id = ?2", options)
	case adversarialUpdateJournalAuthorityAssignmentEpisodesf918:
		return sqlitex.Execute(conn, "UPDATE journal_authority_assignment_episodes SET parent_assignment_id = ?1 WHERE assignment_id = ?2", options)
	case adversarialUpdateTasks4c22:
		return sqlitex.Execute(conn, "UPDATE tasks SET last_journal_id=?1 WHERE id=?2", options)
	case adversarialUpdateTasks9d9f:
		return sqlitex.Execute(conn, "UPDATE tasks SET status_id=?1 WHERE id=?2", options)
	case adversarialUpdateTasksc047:
		return sqlitex.Execute(conn, "UPDATE tasks SET owner_id=?1 WHERE id=?2", options)
	default:
		return unknownSQLStatementError("adversarialStatement", uint16(statement))
	}
}
