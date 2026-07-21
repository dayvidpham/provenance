package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type operationsStatement uint16

const (
	operationsDeleteEdges21cd operationsStatement = iota + 1
	operationsDeleteLabels0ffc
	operationsDeleteShadowEdges592b
	operationsDeleteShadowLabelsa2f4
	operationsInsertAssignmentSlots8738
	operationsInsertAssignmentTransitions2b4e
	operationsInsertAuthorityKinds9817
	operationsInsertComments2a26
	operationsInsertEdges0526
	operationsInsertJournalAuthorityAssignmentEpisodes9fae
	operationsInsertJournalDecisions9b15
	operationsInsertJournalEvidence209d
	operationsInsertJournalNew67f9
	operationsInsertJournalOperationResultSlots9f08
	operationsInsertJournalOperations35a2
	operationsInsertJournalOperationsGeneric860f
	operationsInsertJournalTaskEventContexts970f
	operationsInsertJournalTaskEventContextseca5
	operationsInsertLabels502e
	operationsInsertShadowComments3660
	operationsInsertShadowEdges93a0
	operationsInsertShadowLabelsc0c1
	operationsInsertShadowTaskAttributions58d6
	operationsInsertTaskAttributions0f2b
	operationsInsertTasks4540
	operationsSelectJournal476e
	operationsSelectJournal5e6e
	operationsSelectJournalAuthorities0b4b
	operationsSelectJournalAuthoritiescf26
	operationsSelectJournalAuthoritiesd4e5
	operationsSelectJournalAuthorityAssignmentEpisodes036e
	operationsSelectJournalAuthorityAssignmentEpisodes317f
	operationsSelectJournalAuthorityAssignmentEpisodes77f5
	operationsSelectJournalAuthorityAssignmentEpisodes7d4e
	operationsSelectJournalAuthorityAssignmentEpisodes89e2
	operationsSelectJournalAuthorityAssignmentEpisodescfad
	operationsSelectJournalAuthorityAssignmentEpisodesddfc
	operationsSelectJournalAuthorityAssignmentEpisodesf2d6
	operationsSelectJournalAuthorityAssignmentTransitions5f45
	operationsSelectJournalAuthorityAssignmentTransitionsd830
	operationsSelectJournalAuthorityAssignmentTransitionse5c0
	operationsSelectJournalOperationResultSlots31e2
	operationsSelectJournalOperations08dc
	operationsSelectJournalOperations35e7
	operationsSelectJournalOperations9050
	operationsSelectJournalOperations9c35
	operationsSelectJournalOperationsfa92
	operationsSelectJournala2a6
	operationsSelectJournala8d9
	operationsSelectSqliteMasterebbe
	operationsSelectTasks26a5
	operationsUpdateShadowTasks2fcd
	operationsUpdateShadowTaskscb87
	operationsUpdateTasks1ef4
	operationsUpdateTasks8e93
	operationsWithEdgesb8dc
	operationsWithShadowEdgesde79
)

func (operationsStatement) statementClass() sqlStatementClass { return sqlDMLStatement }

func (statement operationsStatement) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case operationsDeleteEdges21cd:
		return sqlitex.Execute(conn, "DELETE FROM edges WHERE source_id=?1 AND target_id=?2 AND kind_id=?3", options)
	case operationsDeleteLabels0ffc:
		return sqlitex.Execute(conn, "DELETE FROM labels WHERE task_id=?1 AND name=?2", options)
	case operationsDeleteShadowEdges592b:
		return sqlitex.Execute(conn, "DELETE FROM shadow_edges WHERE source_id=?1 AND target_id=?2 AND kind_id=?3", options)
	case operationsDeleteShadowLabelsa2f4:
		return sqlitex.Execute(conn, "DELETE FROM shadow_labels WHERE task_id=?1 AND name=?2", options)
	case operationsInsertAssignmentSlots8738:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO assignment_slots (id, name) VALUES (?1, ?2)", options)
	case operationsInsertAssignmentTransitions2b4e:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO assignment_transitions (id, name) VALUES (?1, ?2)", options)
	case operationsInsertAuthorityKinds9817:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO authority_kinds (id, name) VALUES (?1, ?2)", options)
	case operationsInsertComments2a26:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO comments (id,task_id,author_id,body,created_at) VALUES (?1,?2,?3,?4,?5)", options)
	case operationsInsertEdges0526:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO edges (source_id,target_id,kind_id,created_at) VALUES (?1,?2,?3,?4)", options)
	case operationsInsertJournalAuthorityAssignmentEpisodes9fae:
		return sqlitex.Execute(conn, "INSERT INTO journal_authority_assignment_episodes (assignment_id, task_id, slot_id, actor_id, predecessor_assignment_id, parent_assignment_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6)", options)
	case operationsInsertJournalDecisions9b15:
		return sqlitex.Execute(conn, "INSERT INTO journal_decisions (journal_id, decision_kind, task_id, payload) VALUES (?1, ?2, ?3, ?4)", options)
	case operationsInsertJournalEvidence209d:
		return sqlitex.Execute(conn, "INSERT INTO journal_evidence (journal_id, evidence_kind, task_id, content_digest, payload) VALUES (?1, ?2, ?3, ?4, ?5)", options)
	case operationsInsertJournalNew67f9:
		return sqlitex.Execute(conn, "INSERT INTO journal_new (journal_id, kind_id, actor_id, recorded_at, produced_by_operation_journal_id)\n\t\t\tSELECT journal_id, kind_id, actor_id, recorded_at, produced_by_operation_journal_id FROM journal", options)
	case operationsInsertJournalOperationResultSlots9f08:
		return sqlitex.Execute(conn, "INSERT INTO journal_operation_result_slots (journal_id, result_slot_id, produced_journal_id) VALUES (?1, ?2, ?3)", options)
	case operationsInsertJournalOperations35a2:
		return sqlitex.Execute(conn, "INSERT INTO journal_operations\n\t\t (journal_id, operation_id, authority_journal_id, command_digest, mutation_digest, mutation_encoding_version, canonical_mutation)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)", options)
	case operationsInsertJournalOperationsGeneric860f:
		return sqlitex.Execute(conn, "INSERT INTO journal_operations_generic SELECT * FROM journal_operations", options)
	case operationsInsertJournalTaskEventContexts970f:
		return sqlitex.Execute(conn, "INSERT INTO journal_task_event_contexts (event_journal_id, context_kind, context_identity, attached_by_journal_id) VALUES (?1, ?2, ?3, ?4)", options)
	case operationsInsertJournalTaskEventContextseca5:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO journal_task_event_contexts (event_journal_id, context_kind, context_identity, attached_by_journal_id)\n\t\t\t VALUES (?1, ?2, ?3, ?4)", options)
	case operationsInsertLabels502e:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO labels (task_id,name) VALUES (?1,?2)", options)
	case operationsInsertShadowComments3660:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO shadow_comments (id,task_id,author_id,body,created_at) VALUES (?1,?2,?3,?4,?5)", options)
	case operationsInsertShadowEdges93a0:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO shadow_edges (source_id,target_id,kind_id,created_at) VALUES (?1,?2,?3,?4)", options)
	case operationsInsertShadowLabelsc0c1:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO shadow_labels (task_id,name) VALUES (?1,?2)", options)
	case operationsInsertShadowTaskAttributions58d6:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO shadow_task_attributions (task_id, actor_id, first_journal_id) VALUES (?1, ?2, ?3)", options)
	case operationsInsertTaskAttributions0f2b:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO task_attributions (task_id, actor_id, first_journal_id) VALUES (?1, ?2, ?3)", options)
	case operationsInsertTasks4540:
		return sqlitex.Execute(conn, "INSERT INTO tasks\n\t\t\t(id, namespace, title, description, status_id, priority_id, type_id,\n\t\t\t phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason, last_journal_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?13, ?9, ?10, ?11, ?14, ?9, ?12)", appendStaticSQLArgs(options, nil, nil))
	case operationsSelectJournal476e:
		return sqlitex.Execute(conn, "SELECT ?1 FROM journal LIMIT ?2", appendStaticSQLArgs(options, 1, 1))
	case operationsSelectJournal5e6e:
		return sqlitex.Execute(conn, "SELECT produced_by_operation_journal_id FROM journal WHERE journal_id = ?1", options)
	case operationsSelectJournalAuthorities0b4b:
		return sqlitex.Execute(conn, "SELECT authority_kind_id FROM journal_authorities WHERE journal_id = ?1", options)
	case operationsSelectJournalAuthoritiescf26:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal_authorities WHERE authority_kind_id = ?1", options)
	case operationsSelectJournalAuthoritiesd4e5:
		return sqlitex.Execute(conn, "SELECT ?2 FROM journal_authorities WHERE journal_id = ?1", appendStaticSQLArgs(options, 1))
	case operationsSelectJournalAuthorityAssignmentEpisodes036e:
		return sqlitex.Execute(conn, "SELECT assignment_id FROM journal_authority_assignment_episodes WHERE task_id = ?1", options)
	case operationsSelectJournalAuthorityAssignmentEpisodes317f:
		return sqlitex.Execute(conn, "SELECT parent_assignment_id FROM journal_authority_assignment_episodes WHERE assignment_id = ?1", options)
	case operationsSelectJournalAuthorityAssignmentEpisodes77f5:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal_authority_assignment_episodes", options)
	case operationsSelectJournalAuthorityAssignmentEpisodes7d4e:
		return sqlitex.Execute(conn, "SELECT e.actor_id FROM journal_authority_assignment_episodes e\n\t\t JOIN journal_authority_assignment_transitions started\n\t\t   ON started.assignment_id = e.assignment_id AND started.transition_id = ?2\n\t\t WHERE e.task_id = ?1 AND e.slot_id = ?3\n\t\t   AND NOT EXISTS (SELECT ?5 FROM journal_authority_assignment_transitions ended\n\t\t                   WHERE ended.assignment_id = e.assignment_id AND ended.transition_id = ?4)\n\t\t ORDER BY started.journal_id DESC LIMIT ?6", appendStaticSQLArgs(options, 1, 1))
	case operationsSelectJournalAuthorityAssignmentEpisodes89e2:
		return sqlitex.Execute(conn, "SELECT ?5 FROM journal_authority_assignment_episodes e\n\t\t WHERE e.task_id = ?1 AND e.slot_id = ?2\n\t\t   AND EXISTS (SELECT ?6 FROM journal_authority_assignment_transitions s WHERE s.assignment_id = e.assignment_id AND s.transition_id = ?3)\n\t\t   AND NOT EXISTS (SELECT ?7 FROM journal_authority_assignment_transitions x WHERE x.assignment_id = e.assignment_id AND x.transition_id = ?4)\n\t\t LIMIT ?8", appendStaticSQLArgs(options, 1, 1, 1, 1))
	case operationsSelectJournalAuthorityAssignmentEpisodescfad:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal_authority_assignment_episodes WHERE task_id = ?1 AND predecessor_assignment_id IS NOT ?2", appendStaticSQLArgs(options, nil))
	case operationsSelectJournalAuthorityAssignmentEpisodesddfc:
		return sqlitex.Execute(conn, "SELECT ?2 FROM journal_authority_assignment_episodes WHERE assignment_id = ?1", appendStaticSQLArgs(options, 1))
	case operationsSelectJournalAuthorityAssignmentEpisodesf2d6:
		return sqlitex.Execute(conn, "SELECT task_id FROM journal_authority_assignment_episodes WHERE assignment_id = ?1", options)
	case operationsSelectJournalAuthorityAssignmentTransitions5f45:
		return sqlitex.Execute(conn, "SELECT assignment_id FROM journal_authority_assignment_transitions WHERE journal_id = ?1", options)
	case operationsSelectJournalAuthorityAssignmentTransitionsd830:
		return sqlitex.Execute(conn, "SELECT ?4 FROM journal_authority_assignment_transitions\n\t\t WHERE assignment_id = ?1 AND transition_id = ?2 AND journal_id < ?3 LIMIT ?5", appendStaticSQLArgs(options, 1, 1))
	case operationsSelectJournalAuthorityAssignmentTransitionse5c0:
		return sqlitex.Execute(conn, "SELECT ?3 FROM journal_authority_assignment_transitions WHERE assignment_id = ?1 AND transition_id = ?2", appendStaticSQLArgs(options, 1))
	case operationsSelectJournalOperationResultSlots31e2:
		return sqlitex.Execute(conn, "SELECT s.result_slot_id, s.produced_journal_id, j.kind_id, te.task_id\n\t\t FROM journal_operation_result_slots s\n\t\t JOIN journal j ON j.journal_id = s.produced_journal_id\n\t\t LEFT JOIN journal_task_events te ON te.journal_id = s.produced_journal_id\n\t\t WHERE s.journal_id = ?1 ORDER BY s.result_slot_id ASC", options)
	case operationsSelectJournalOperations08dc:
		return sqlitex.Execute(conn, "SELECT operation_id FROM journal_operations WHERE canonical_mutation IS NOT ?2 AND length(canonical_mutation)>?1 LIMIT ?3", appendStaticSQLArgs(options, nil, 1))
	case operationsSelectJournalOperations35e7:
		return sqlitex.Execute(conn, "SELECT operation_id,mutation_encoding_version,canonical_mutation FROM journal_operations WHERE canonical_mutation IS NOT ?1", appendStaticSQLArgs(options, nil))
	case operationsSelectJournalOperations9050:
		return sqlitex.Execute(conn, "SELECT journal_id, authority_journal_id, command_digest, mutation_digest,\n\t\t        mutation_encoding_version, canonical_mutation\n\t\t FROM journal_operations WHERE operation_id = ?1", options)
	case operationsSelectJournalOperations9c35:
		return sqlitex.Execute(conn, "SELECT operation_id,mutation_encoding_version,canonical_mutation FROM journal_operations WHERE (mutation_encoding_version IS ?1) != (canonical_mutation IS ?2) OR (mutation_encoding_version IS NOT ?3 AND (NOT length(mutation_encoding_version) OR NOT length(canonical_mutation))) LIMIT ?4", appendStaticSQLArgs(options, nil, nil, nil, 1))
	case operationsSelectJournalOperationsfa92:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal_operations", options)
	case operationsSelectJournala2a6:
		return sqlitex.Execute(conn, "SELECT journal_id FROM journal WHERE produced_by_operation_journal_id = ?1 AND kind_id = ?2 ORDER BY journal_id ASC", options)
	case operationsSelectJournala8d9:
		return sqlitex.Execute(conn, "SELECT actor_id FROM journal WHERE journal_id = ?1", options)
	case operationsSelectSqliteMasterebbe:
		return sqlitex.Execute(conn, "SELECT sql FROM sqlite_master WHERE type=?1 AND name=?2", options)
	case operationsSelectTasks26a5:
		return sqlitex.Execute(conn, "SELECT ?2 FROM tasks WHERE id = ?1", appendStaticSQLArgs(options, 1))
	case operationsUpdateShadowTasks2fcd:
		return sqlitex.Execute(conn, "UPDATE shadow_tasks SET last_journal_id = ?1 WHERE id = ?2", options)
	case operationsUpdateShadowTaskscb87:
		return sqlitex.Execute(conn, "UPDATE shadow_tasks SET owner_id = ?1, last_journal_id = ?2 WHERE id = ?3", options)
	case operationsUpdateTasks1ef4:
		return sqlitex.Execute(conn, "UPDATE tasks SET owner_id = ?1, last_journal_id = ?2 WHERE id = ?3", options)
	case operationsUpdateTasks8e93:
		return sqlitex.Execute(conn, "UPDATE tasks SET\n\t\tupdated_at=?1,\n\t\ttitle=CASE WHEN ?2 THEN ?3 ELSE title END,\n\t\tdescription=CASE WHEN ?4 THEN ?5 ELSE description END,\n\t\tpriority_id=CASE WHEN ?6 THEN ?7 ELSE priority_id END,\n\t\tphase_id=CASE WHEN ?8 THEN ?9 ELSE phase_id END,\n\t\tnotes=CASE WHEN ?10 THEN ?11 ELSE notes END,\n\t\tclose_reason=CASE WHEN ?12 THEN ?13 ELSE close_reason END\n\t\tWHERE id=?14", options)
	case operationsWithEdgesb8dc:
		return sqlitex.Execute(conn, "WITH RECURSIVE reach(node) AS (SELECT ?1 UNION SELECT e.target_id FROM edges e JOIN reach r ON e.source_id=r.node WHERE e.kind_id=?3) SELECT ?4 FROM reach WHERE node=?2 LIMIT ?5", appendStaticSQLArgs(options, 1, 1))
	case operationsWithShadowEdgesde79:
		return sqlitex.Execute(conn, "WITH RECURSIVE reach(node) AS (SELECT ?1 UNION SELECT e.target_id FROM shadow_edges e JOIN reach r ON e.source_id=r.node WHERE e.kind_id=?3) SELECT ?4 FROM reach WHERE node=?2 LIMIT ?5", appendStaticSQLArgs(options, 1, 1))
	default:
		return unknownSQLStatementError("operationsStatement", uint16(statement))
	}
}
